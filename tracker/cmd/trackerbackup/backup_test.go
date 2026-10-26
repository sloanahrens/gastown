//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		stderr: &bytes.Buffer{},
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
		for _, secret := range []string{testPassword, testDSN, "tracker:s3cr3t"} {
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
	err := b.prune(testNow)
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
	dumps := dumpsIn(t, dir)
	if len(dumps) != 1 {
		t.Fatalf("-dir holds dumps %v, want one", dumps)
	}
	if !strings.Contains(stdout.String(), dumps[0]) {
		t.Errorf("stdout = %q, want it to name %s", stdout.String(), dumps[0])
	}
}

func TestRunWithDefaultDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fake := &fakeDump{body: []byte("dump")}
	// The date is read before the run. A midnight that falls between this read
	// and the run's own clock read can still name the next day's dump, so the
	// test assumes the run does not cross midnight, which it never does here.
	today := time.Now().Format(dateLayout)
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
	if dumps := dumpsIn(t, dir); len(dumps) != 1 || dumps[0] != today+dumpSuffix {
		t.Fatalf("%s holds dumps %v, want tonight's dump only", dir, dumps)
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
			name:    "short password is masked wherever it appears",
			text:    "abc is not a valid option",
			secrets: []string{"abc"},
			want:    "*** is not a valid option",
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

// The password is out of argv and in PGPASSWORD, and every other DSN setting,
// sslmode included, reaches pg_dump in the -d argument (gt-p85pw.48).
func TestRunKeepsTheDSNSettingsButNotThePassword(t *testing.T) {
	t.Parallel()
	fake := &fakeDump{body: []byte("dump")}
	b, _ := newBackup(t, fake, 7)
	b.dsn = "postgres://tracker:s3cr3t-Pa55w0rd@db.internal:5433/tracker?sslmode=verify-full"
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	d := argValue(fake.argv, "-d")
	if !strings.Contains(d, "sslmode=verify-full") {
		t.Errorf("-d = %q, want the sslmode setting kept", d)
	}
	if strings.Contains(d, testPassword) {
		t.Errorf("-d = %q holds the password", d)
	}
	if vals := envValues(fake.env, "PGPASSWORD"); len(vals) != 1 || vals[0] != testPassword {
		t.Errorf("PGPASSWORD = %q, want the password once", vals)
	}
}

// A keyword DSN loses only its password setting (gt-p85pw.48).
func TestDsnWithoutPasswordKeywordForm(t *testing.T) {
	t.Parallel()
	got, err := dsnWithoutPassword("host=db port=5433 user=tracker password='s3cr t' dbname=tracker sslmode=require")
	if err != nil {
		t.Fatalf("dsnWithoutPassword: %v", err)
	}
	if strings.Contains(got, "password") || strings.Contains(got, "s3cr") || !strings.Contains(got, "sslmode=require") {
		t.Errorf("keyword DSN without password = %q", got)
	}
}

// A second run in the same directory finds the lock held and writes nothing
// (gt-p85pw.48).
func TestRunRefusesASecondRunInTheSameDir(t *testing.T) {
	t.Parallel()
	fake := &fakeDump{body: []byte("dump")}
	b, _ := newBackup(t, fake, 7)
	unlock, err := lockBackupDir(b.dir)
	if err != nil {
		t.Fatalf("lockBackupDir: %v", err)
	}
	defer unlock()
	err = b.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "holds the lock") {
		t.Fatalf("Run with the lock held = %v, want the lock refusal", err)
	}
	if fake.argv != nil {
		t.Errorf("pg_dump ran while another run held the lock: %q", fake.argv)
	}
}

// A retention failure after the dump is published is a warning, not a failed
// run (gt-p85pw.48).
func TestRunWarnsWhenRetentionFailsAfterPublish(t *testing.T) {
	t.Parallel()
	b, stdout := newBackup(t, &fakeDump{body: []byte("dump")}, 1)
	writeFile(t, b.dir, "2026-01-01.dump", "old")
	b.remove = func(name string) error {
		if strings.HasSuffix(name, "2026-01-01.dump") {
			return errors.New("read-only file system")
		}
		return os.Remove(name)
	}
	var stderr bytes.Buffer
	b.stderr = &stderr
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run = %v, want success: the dump was published", err)
	}
	if !strings.Contains(stderr.String(), "warning") || !strings.Contains(stdout.String(), "wrote") {
		t.Errorf("stderr %q, stdout %q: want the retention warning and the publish line", stderr.String(), stdout.String())
	}
}

// Dumps dated after today are never pruned and do not count toward keep
// (gt-p85pw.48).
func TestPruneIgnoresFutureDumps(t *testing.T) {
	t.Parallel()
	b, _ := newBackup(t, &fakeDump{}, 2)
	writeFile(t, b.dir, "2026-01-07.dump", "older")
	writeFile(t, b.dir, "2026-01-08.dump", "yesterday")
	writeFile(t, b.dir, "2026-01-09.dump", "today")
	writeFile(t, b.dir, "2026-02-01.dump", "future")
	if err := b.prune(testNow); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, gone := range []string{"2026-01-07.dump"} {
		if _, err := os.Stat(filepath.Join(b.dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived keep=2", gone)
		}
	}
	for _, kept := range []string{"2026-01-08.dump", "2026-01-09.dump", "2026-02-01.dump"} {
		if _, err := os.Stat(filepath.Join(b.dir, kept)); err != nil {
			t.Errorf("%s was pruned: %v", kept, err)
		}
	}
}

// The real execution path: execRunner runs a child, passes its environment,
// and reports its failure. The child is this test binary, re-entered through
// TestMain, so no shell or pg_dump is needed (gt-p85pw.48).
func TestExecRunnerRunsTheChild(t *testing.T) {
	t.Parallel()
	partial := filepath.Join(t.TempDir(), "d.dump.partial")
	argv := []string{os.Args[0], "--", "-f", partial}
	env := []string{"GO_WANT_HELPER_PROCESS=1"}
	if err := (execRunner{}).Run(context.Background(), argv, env); err != nil {
		t.Fatalf("execRunner.Run: %v", err)
	}
	if info, err := os.Stat(partial); err != nil || info.Size() == 0 {
		t.Fatalf("child did not write the dump: %v", err)
	}

	failArgv := []string{os.Args[0], "--", "-f", partial}
	err := (execRunner{}).Run(context.Background(), failArgv, []string{"GO_WANT_HELPER_PROCESS=1", "GO_HELPER_FAIL=1"})
	if err == nil || !strings.Contains(err.Error(), "helper: no tracker") {
		t.Errorf("failing child = %v, want its last stderr line", err)
	}
}

// TestMain re-enters this binary as a fake pg_dump when GO_WANT_HELPER_PROCESS
// is set: a child process that writes -f, or fails with a stderr line when
// GO_HELPER_FAIL is set. Otherwise the tests run as usual (gt-p85pw.48).
func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		if os.Getenv("GO_HELPER_FAIL") == "1" {
			fmt.Fprintln(os.Stderr, "helper: no tracker database")
			os.Exit(1)
		}
		if err := os.WriteFile(argValue(os.Args, "-f"), []byte("dump"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The whole run with the real exec runner: -pg-bin names a directory holding
// this test binary as pg_dump, so the run starts a real child and reads the
// dump it wrote. The test sets an environment variable, so it does not run in
// parallel (gt-p85pw.48).
func TestRunEndToEndWithTheExecRunner(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pgBin := t.TempDir()
	if err := os.Symlink(self, filepath.Join(pgBin, "pg_dump")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	today := time.Now().Format(dateLayout)
	getenv := func(k string) string {
		if k == "TRACKER_DSN" {
			return testDSN
		}
		return ""
	}
	var stdout, stderr bytes.Buffer
	if err := runWith(context.Background(), []string{"-dir", dir, "-pg-bin", pgBin}, getenv, &stdout, &stderr, execRunner{}); err != nil {
		t.Fatalf("runWith with the exec runner: %v (stderr %q)", err, stderr.String())
	}
	if got := dumpsIn(t, dir); len(got) != 1 || got[0] != today+dumpSuffix {
		t.Fatalf("dumps = %v, want tonight's dump", got)
	}
}

// A password in the query string is removed too (gt-p85pw.48).
func TestDsnWithoutPasswordQueryForm(t *testing.T) {
	t.Parallel()
	got, err := dsnWithoutPassword("postgres://tracker:uripw@db.internal/tracker?sslmode=require&password=qpw")
	if err != nil {
		t.Fatalf("dsnWithoutPassword: %v", err)
	}
	if strings.Contains(got, "qpw") || strings.Contains(got, "uripw") || !strings.Contains(got, "sslmode=require") {
		t.Errorf("URI DSN without password = %q", got)
	}
}

// dumpsIn lists the dump files in dir, leaving out the run lock.
func dumpsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), dumpSuffix) {
			names = append(names, e.Name())
		}
	}
	return names
}

// An escaped quote inside a quoted keyword password is one value, so no part
// of it is left in argv (gt-p85pw.48).
func TestDsnWithoutPasswordEscapedQuote(t *testing.T) {
	t.Parallel()
	got, err := dsnWithoutPassword(`host=db password='pa\'ss word' sslmode=require`)
	if err != nil {
		t.Fatalf("dsnWithoutPassword: %v", err)
	}
	if strings.Contains(got, "word") || strings.Contains(got, `\'`) || !strings.Contains(got, "sslmode=require") {
		t.Errorf("keyword DSN with an escaped quote = %q", got)
	}
}

// A dump dated after today is named on stderr, and it is never pruned.
func TestPruneWarnsAboutFutureDumps(t *testing.T) {
	t.Parallel()
	b, _ := newBackup(t, &fakeDump{}, 7)
	var stderr bytes.Buffer
	b.stderr = &stderr
	writeFile(t, b.dir, "2026-02-01.dump", "future")
	if err := b.prune(testNow); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !strings.Contains(stderr.String(), "2026-02-01.dump") {
		t.Errorf("stderr = %q, want a warning that names the future dump", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(b.dir, "2026-02-01.dump")); err != nil {
		t.Errorf("future dump was removed: %v", err)
	}
}
