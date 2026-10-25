package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testDSN      = "postgres://tracker:s3cr3t-Pa55w0rd@db.internal:5433/tracker"
	testPassword = "s3cr3t-Pa55w0rd"
)

// testNow is the 03:00 run on 2026-01-09; the date it names is 2026-01-09.
var testNow = time.Date(2026, 1, 9, 3, 0, 0, 0, time.Local)

// fakeDump stands in for pg_dump: it records the argv and environment it was
// called with and does to the -f file what the test asked for.
type fakeDump struct {
	argv         []string
	env          []string
	calls        int
	body         []byte
	empty        bool
	removeTarget bool
	fail         error
}

func (f *fakeDump) Run(_ context.Context, argv, env []string) error {
	f.calls++
	f.argv, f.env = argv, env
	target := argValue(argv, "-f")
	if target == "" {
		return errors.New("fake pg_dump: no -f in argv")
	}
	if !f.empty {
		if err := os.WriteFile(target, f.body, 0o600); err != nil {
			return err
		}
	}
	if f.removeTarget {
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	return f.fail
}

// argValue is the argument after name, or "" when name is absent.
func argValue(argv []string, name string) string {
	for i, a := range argv {
		if a == name && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// envValues is every value the environment gives name.
func envValues(env []string, name string) []string {
	var vals []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			vals = append(vals, v)
		}
	}
	return vals
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// newBackup returns a backup over a fresh temporary directory, wired to the
// fake runner and a fixed clock.
func newBackup(t *testing.T, fake *fakeDump, keep int) (*backup, *bytes.Buffer) {
	t.Helper()
	var stdout bytes.Buffer
	return &backup{
		dir:    t.TempDir(),
		keep:   keep,
		pgBin:  "/fixture/pg/bin",
		dsn:    testDSN,
		clock:  fixedClock{testNow},
		run:    fake,
		remove: os.Remove,
		stdout: &stdout,
	}, &stdout
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestRunWritesDatedDump(t *testing.T) {
	t.Parallel()
	fake := &fakeDump{body: []byte("custom-format dump")}
	b, stdout := newBackup(t, fake, 7)
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	target := filepath.Join(b.dir, "2026-01-09.dump")
	partial := filepath.Join(b.dir, "2026-01-09.dump.partial")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	if string(got) != "custom-format dump" {
		t.Errorf("dump body = %q, want what pg_dump wrote", got)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Errorf("a .partial file survived a successful run (stat err = %v)", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat dump: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("dump mode = %04o, want 0600", perm)
	}

	if want := filepath.Join(b.pgBin, "pg_dump"); fake.argv[0] != want {
		t.Errorf("argv[0] = %q, want %q", fake.argv[0], want)
	}
	if !slices.Contains(fake.argv, "-Fc") {
		t.Errorf("argv = %q, want the custom format flag -Fc", fake.argv)
	}
	if got := argValue(fake.argv, "-f"); got != partial {
		t.Errorf("pg_dump wrote %q, want the partial file %q", got, partial)
	}

	// argv is visible to every local user in a process listing, so the
	// credentials must not be in it.
	for _, a := range fake.argv {
		for _, secret := range []string{testPassword, testDSN, "postgres://"} {
			if strings.Contains(a, secret) {
				t.Errorf("argv %q holds credential material %q", a, secret)
			}
		}
	}
	for name, want := range map[string]string{
		"PGHOST":     "db.internal",
		"PGPORT":     "5433",
		"PGUSER":     "tracker",
		"PGDATABASE": "tracker",
		"PGPASSWORD": testPassword,
	} {
		vals := envValues(fake.env, name)
		if len(vals) != 1 || vals[0] != want {
			t.Errorf("%s = %q in the pg_dump environment, want exactly %q", name, vals, want)
		}
	}
	// The child reads the settings above and has no use for the whole DSN.
	for _, kv := range fake.env {
		if strings.Contains(kv, testDSN) {
			t.Errorf("pg_dump environment entry %q carries the DSN", kv)
		}
	}
	if !strings.Contains(stdout.String(), target) {
		t.Errorf("stdout = %q, want it to name the dump %s", stdout.String(), target)
	}
}

func TestRunPrunesToKeep(t *testing.T) {
	t.Parallel()
	b, _ := newBackup(t, &fakeDump{body: []byte("tonight")}, 7)
	for day := 1; day <= 8; day++ {
		writeFile(t, b.dir, time.Date(2026, 1, day, 3, 0, 0, 0, time.Local).Format(dateLayout)+dumpSuffix, "old dump")
	}
	writeFile(t, b.dir, "notes.txt", "not a dump")
	writeFile(t, b.dir, "2026-02-30.dump", "no such day")
	writeFile(t, b.dir, "2026-1-1.dump", "wrong shape")

	// Nine dated dumps after the write, seven kept: the two oldest go.
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, name := range []string{"2026-01-01.dump", "2026-01-02.dump"} {
		if _, err := os.Stat(filepath.Join(b.dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned (stat err = %v)", name, err)
		}
	}
	for day := 3; day <= 9; day++ {
		name := time.Date(2026, 1, day, 3, 0, 0, 0, time.Local).Format(dateLayout) + dumpSuffix
		if _, err := os.Stat(filepath.Join(b.dir, name)); err != nil {
			t.Errorf("%s should be kept: %v", name, err)
		}
	}
	for _, name := range []string{"notes.txt", "2026-02-30.dump", "2026-1-1.dump"} {
		if _, err := os.Stat(filepath.Join(b.dir, name)); err != nil {
			t.Errorf("%s is not a dump this program wrote and must be kept: %v", name, err)
		}
	}
}

func TestRunRemovesStalePartials(t *testing.T) {
	t.Parallel()
	b, _ := newBackup(t, &fakeDump{body: []byte("tonight")}, 7)
	writeFile(t, b.dir, "2026-01-05.dump.partial", "half a dump from a killed run")
	writeFile(t, b.dir, "2026-02-30.dump.partial", "not a date")
	writeFile(t, b.dir, "notes.txt.partial", "not a dump name")
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, name := range []string{"2026-01-05.dump.partial"} {
		if _, err := os.Stat(filepath.Join(b.dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed (stat err = %v)", name, err)
		}
	}
	for _, name := range []string{"2026-02-30.dump.partial", "notes.txt.partial"} {
		if _, err := os.Stat(filepath.Join(b.dir, name)); err != nil {
			t.Errorf("%s does not match a dated run and must be kept: %v", name, err)
		}
	}
}

func TestRunFailureKeepsOlderDumpsAndLeavesNoPartial(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fake *fakeDump
		want string
	}{
		{
			name: "pg_dump fails",
			fake: &fakeDump{body: []byte("half a dump"), fail: errors.New("connection refused: " + testDSN)},
			want: "pg_dump: connection refused: ***",
		},
		{name: "empty dump", fake: &fakeDump{empty: true}, want: "empty dump"},
		{name: "no dump file", fake: &fakeDump{body: []byte("x"), removeTarget: true}, want: "wrote no dump"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b, _ := newBackup(t, c.fake, 7)
			writeFile(t, b.dir, "2026-01-08.dump", "yesterday")
			err := b.Run(context.Background())
			if err == nil {
				t.Fatalf("Run succeeded, want an error naming %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to name %q", err, c.want)
			}
			for _, secret := range []string{testPassword, testDSN} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks %q: %q", secret, err)
				}
			}
			got, err := os.ReadFile(filepath.Join(b.dir, "2026-01-08.dump"))
			if err != nil || string(got) != "yesterday" {
				t.Errorf("the older dump changed: %q, %v", got, err)
			}
			for _, name := range []string{"2026-01-09.dump", "2026-01-09.dump.partial"} {
				if _, err := os.Stat(filepath.Join(b.dir, name)); !os.IsNotExist(err) {
					t.Errorf("%s must not exist after a failed run (stat err = %v)", name, err)
				}
			}
		})
	}
}

func TestRunReplacesSameDayDumpOnlyAfterSuccess(t *testing.T) {
	t.Parallel()
	fake := &fakeDump{body: []byte("first attempt")}
	b, _ := newBackup(t, fake, 7)
	target := filepath.Join(b.dir, "2026-01-09.dump")

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	fake.body, fake.fail = []byte("second attempt"), errors.New("boom")
	if err := b.Run(context.Background()); err == nil {
		t.Fatal("second Run succeeded, want the pg_dump failure")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "first attempt" {
		t.Fatalf("after a failed rerun the dump is %q (%v), want the first attempt", got, err)
	}

	fake.body, fake.fail = []byte("third attempt"), nil
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("third Run: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "third attempt" {
		t.Errorf("after a successful rerun the dump is %q (%v), want the third attempt", got, err)
	}
}

func TestRunRejectsKeepBelowOne(t *testing.T) {
	t.Parallel()
	fake := &fakeDump{body: []byte("x")}
	b, _ := newBackup(t, fake, 0)
	err := b.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "-keep must be at least 1") {
		t.Fatalf("Run(%d dumps) error = %v, want it to reject -keep 0", 0, err)
	}
	if fake.calls != 0 {
		t.Errorf("pg_dump ran %d times, want none", fake.calls)
	}
}

func TestRunReportsMkdirFailure(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	fake := &fakeDump{body: []byte("x")}
	b, _ := newBackup(t, fake, 7)
	b.dir = filepath.Join(blocker, "tracker")
	err := b.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "create backup directory") {
		t.Fatalf("Run error = %v, want it to name the directory it could not create", err)
	}
	if fake.calls != 0 {
		t.Errorf("pg_dump ran %d times, want none", fake.calls)
	}
}

func TestPruneReportsRemoveFailure(t *testing.T) {
	t.Parallel()
	b, _ := newBackup(t, &fakeDump{}, 1)
	writeFile(t, b.dir, "2026-01-01.dump", "old")
	writeFile(t, b.dir, "2026-01-02.dump", "newer")
	b.remove = func(string) error { return errors.New("read-only file system") }
	err := b.prune()
	if err == nil || !strings.Contains(err.Error(), "delete old dump 2026-01-01.dump") {
		t.Fatalf("prune error = %v, want it to name the dump it could not delete", err)
	}
}

func TestRunWithFlags(t *testing.T) {
	t.Parallel()
	fake := &fakeDump{body: []byte("dump")}
	dir := filepath.Join(t.TempDir(), "dumps")
	bin := t.TempDir()
	var stdout, stderr bytes.Buffer
	getenv := func(k string) string {
		if k == "TRACKER_DSN" {
			return testDSN
		}
		return ""
	}
	if err := runWith(context.Background(), []string{"-dir", dir, "-keep", "2", "-pg-bin", bin}, getenv, &stdout, &stderr, fake); err != nil {
		t.Fatalf("runWith: %v (stderr %q)", err, stderr.String())
	}
	if want := filepath.Join(bin, "pg_dump"); fake.argv[0] != want {
		t.Errorf("argv[0] = %q, want -pg-bin to choose %q", fake.argv[0], want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read -dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("-dir holds %d entries, want one dump", len(entries))
	}
	if !strings.Contains(stdout.String(), entries[0].Name()) {
		t.Errorf("stdout = %q, want it to name %s", stdout.String(), entries[0].Name())
	}
}

func TestRunWithDefaultDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fake := &fakeDump{body: []byte("dump")}
	var stdout, stderr bytes.Buffer
	getenv := func(k string) string {
		if k == "TRACKER_DSN" {
			return testDSN
		}
		return ""
	}
	if err := runWith(context.Background(), []string{"-pg-bin", t.TempDir()}, getenv, &stdout, &stderr, fake); err != nil {
		t.Fatalf("runWith: %v (stderr %q)", err, stderr.String())
	}
	dir := filepath.Join(home, "gt-backups", "tracker")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("default -dir %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != time.Now().Format(dateLayout)+dumpSuffix {
		t.Fatalf("%s holds %v, want tonight's dump only", dir, entries)
	}
}

func TestRunRequiresDSN(t *testing.T) {
	t.Parallel()
	err := runWith(context.Background(), nil, func(string) string { return "" }, &bytes.Buffer{}, &bytes.Buffer{}, &fakeDump{})
	if err == nil || !strings.Contains(err.Error(), "TRACKER_DSN is not set") {
		t.Fatalf("runWith error = %v, want it to name the missing TRACKER_DSN", err)
	}
}

func TestParseDSN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		dsn     string
		want    credentials
		wantErr string
	}{
		{
			name: "URI",
			dsn:  "postgres://tracker:p%40ss@db.internal:5433/tracker",
			want: credentials{host: "db.internal", port: "5433", user: "tracker", database: "tracker", password: "p@ss"},
		},
		{
			name: "keyword string with a quoted password",
			dsn:  "host=db.internal port=5433 user=tracker password='two words' dbname=tracker",
			want: credentials{host: "db.internal", port: "5433", user: "tracker", database: "tracker", password: "two words"},
		},
		{name: "empty", dsn: "", wantErr: "TRACKER_DSN is not set"},
		{name: "bad port", dsn: "postgres://tracker:hunter2@db.internal:notaport/tracker", wantErr: "cannot use TRACKER_DSN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDSN(c.dsn)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("parseDSN(%q) error = %v, want %q", c.dsn, err, c.wantErr)
				}
				if strings.Contains(err.Error(), "hunter2") || (c.dsn != "" && strings.Contains(err.Error(), c.dsn)) {
					t.Errorf("the parse error quotes the DSN: %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDSN(%q): %v", c.dsn, err)
			}
			if got != c.want {
				t.Errorf("parseDSN(%q) = %+v, want %+v", c.dsn, got, c.want)
			}
		})
	}
}

func TestDumpEnvDropsInheritedSettings(t *testing.T) {
	t.Parallel()
	inherited := []string{
		"PATH=/usr/bin",
		"HOME=/home/tracker",
		"PGHOST=stale",
		"PGPASSWORD=stale-password",
		"PGSERVICE=town",
		"PGSERVICEFILE=/tmp/service.conf",
		"TRACKER_DSN=" + testDSN,
	}
	env := dumpEnv(inherited, credentials{host: "db.internal", port: "5433", user: "tracker", database: "tracker", password: testPassword})
	for _, kv := range inherited[:2] {
		if !slices.Contains(env, kv) {
			t.Errorf("%s was dropped from the child environment, want it kept", kv)
		}
	}
	for _, stale := range inherited[2:] {
		if slices.Contains(env, stale) {
			t.Errorf("%s reaches pg_dump and can override the DSN, want it dropped", stale)
		}
	}
	for name, want := range map[string]string{
		"PGHOST":     "db.internal",
		"PGPORT":     "5433",
		"PGUSER":     "tracker",
		"PGDATABASE": "tracker",
		"PGPASSWORD": testPassword,
	} {
		if vals := envValues(env, name); len(vals) != 1 || vals[0] != want {
			t.Errorf("%s = %q, want exactly %q", name, vals, want)
		}
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()
	dsn := "postgres://tracker:" + testPassword + "@db.internal:5433/tracker"
	cases := []struct {
		name    string
		text    string
		secrets []string
		want    string
	}{
		{
			name:    "bare password",
			text:    "authentication failed for " + testPassword,
			secrets: []string{testPassword},
			want:    "authentication failed for ***",
		},
		{
			name:    "password inside an echoed DSN",
			text:    "could not connect to " + dsn,
			secrets: []string{testPassword},
			want:    "could not connect to postgres://tracker:***@db.internal:5433/tracker",
		},
		{
			name:    "whole DSN",
			text:    "could not connect to " + dsn,
			secrets: []string{dsn, testPassword},
			want:    "could not connect to ***",
		},
		{
			name:    "percent-encoded password",
			text:    "user:p%40ss+word%21 rejected",
			secrets: []string{"p@ss word!"},
			want:    "user:*** rejected",
		},
		{
			name: "quoted keyword password",
			text: "could not translate host name: password='two words'",
			want: "could not translate host name: password=***",
		},
		{
			name: "URI credentials",
			text: "postgres://tracker:hunter2@db.internal/tracker",
			want: "postgres://tracker:***@db.internal/tracker",
		},
		{
			name:    "short password leaves the prose alone",
			text:    "abc is not a valid option",
			secrets: []string{"abc"},
			want:    "abc is not a valid option",
		},
		{
			name:    "short password in credential position",
			text:    "postgres://tracker:abc@db.internal/tracker",
			secrets: []string{"abc"},
			want:    "postgres://tracker:***@db.internal/tracker",
		},
		{
			name:    "empty password",
			text:    "unchanged",
			secrets: []string{""},
			want:    "unchanged",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := redact(c.text, c.secrets...); got != c.want {
				t.Errorf("redact(%q) = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

func TestDumpDate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string // the date in YYYY-MM-DD, or "" for "not a dump"
	}{
		{"2026-01-09.dump", "2026-01-09"},
		{"2026-02-30.dump", ""},
		{"2026-13-01.dump", ""},
		{"2026-1-1.dump", ""},
		{"2026-01-09.dump.partial", ""},
		{"notes.txt", ""},
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := ""
			if d := dumpDate(c.name); !d.IsZero() {
				got = d.Format(dateLayout)
			}
			if got != c.want {
				t.Errorf("dumpDate(%q) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

func TestLastLine(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 500)
	cases := []struct{ name, output, want string }{
		{"empty", "", ""},
		{"one line", "failed\n", "failed"},
		{"last line wins", "pg_dump: warning: something\npg_dump: error: it broke\n", "pg_dump: error: it broke"},
		{"trailing blank lines", "it broke\n\n\n", "it broke"},
		{"long line is capped", long, long[:400]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := lastLine(c.output); got != c.want {
				t.Errorf("lastLine(...) = %q, want %q", got, c.want)
			}
		})
	}
}
