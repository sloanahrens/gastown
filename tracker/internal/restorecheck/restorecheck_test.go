package restorecheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records every command the check runs, the environment it ran it
// with, and whether the context it was given was still live.
type fakeRunner struct {
	calls   *[]string
	cmds    [][]string
	envs    [][]string
	ctxErrs []error
	// fail is the error the named command returns; the command still counts as
	// run.
	fail map[string]error
}

func newFakeRunner(calls *[]string) *fakeRunner {
	return &fakeRunner{calls: calls, fail: map[string]error{}}
}

func (f *fakeRunner) Run(ctx context.Context, argv, env []string) error {
	name := filepath.Base(argv[0])
	*f.calls = append(*f.calls, name)
	f.cmds = append(f.cmds, argv)
	f.envs = append(f.envs, env)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	return f.fail[name]
}

// dumpFile writes a file standing in for a pg_dump, which the check only stats.
func dumpFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "2026-10-28.dump")
	if err := os.WriteFile(path, []byte("dump"), 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}
	return path
}

// fixedName is a Config.Name that always returns name.
func fixedName(name string) func() string { return func() string { return name } }

// config is a check that runs against nothing: the commands are faked and the
// read is recorded, not made.
func config(t *testing.T, calls *[]string, read func(context.Context, string) error) Config {
	t.Helper()
	return Config{
		DSN:   "host=db port=5433 user=tracker dbname=tracker sslmode=require",
		Dump:  dumpFile(t),
		PGBin: "/pg/bin",
		Name:  fixedName("restorecheck_probe"),
		Read: func(ctx context.Context, dsn string) error {
			*calls = append(*calls, "read")
			if read == nil {
				return nil
			}
			return read(ctx, dsn)
		},
		Env: []string{"PATH=/usr/bin", "HOME=/home/tracker"},
	}
}

// A clean run creates the copy, restores into it, reads it and drops it, in
// that order (gt-p85pw.72).
func TestCheckRestoresReadsThenDrops(t *testing.T) {
	t.Parallel()
	var calls []string
	r := newFakeRunner(&calls)
	if err := Check(context.Background(), r, config(t, &calls, nil)); err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := []string{"createdb", "pg_restore", "read", "dropdb"}
	if strings.Join(calls, " ") != strings.Join(want, " ") {
		t.Errorf("commands = %v, want %v", calls, want)
	}
}

// A read that fails still leaves no copy behind, and the failure is the one
// reported (gt-p85pw.72).
func TestCheckDropsTheCopyWhenTheReadFails(t *testing.T) {
	t.Parallel()
	var calls []string
	r := newFakeRunner(&calls)
	readErr := errors.New("the restored copy holds no issue")
	err := Check(context.Background(), r, config(t, &calls, func(context.Context, string) error { return readErr }))
	if !errors.Is(err, readErr) {
		t.Fatalf("Check = %v, want the read failure", err)
	}
	want := []string{"createdb", "pg_restore", "read", "dropdb"}
	if strings.Join(calls, " ") != strings.Join(want, " ") {
		t.Errorf("commands = %v, want %v", calls, want)
	}
}

// A restore that fails leaves no copy behind and the read never runs.
func TestCheckDropsTheCopyWhenTheRestoreFails(t *testing.T) {
	t.Parallel()
	var calls []string
	r := newFakeRunner(&calls)
	r.fail["pg_restore"] = errors.New("exit status 1")
	err := Check(context.Background(), r, config(t, &calls, nil))
	if err == nil || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("Check = %v, want a restore failure", err)
	}
	want := []string{"createdb", "pg_restore", "dropdb"}
	if strings.Join(calls, " ") != strings.Join(want, " ") {
		t.Errorf("commands = %v, want %v", calls, want)
	}
}

// A create that fails drops nothing: the name it was given may belong to a
// database this run never made (gt-p85pw.72).
func TestCheckDropsNothingWhenTheCreateFails(t *testing.T) {
	t.Parallel()
	var calls []string
	r := newFakeRunner(&calls)
	r.fail["createdb"] = errors.New("database already exists")
	err := Check(context.Background(), r, config(t, &calls, nil))
	if err == nil || !strings.Contains(err.Error(), "create") {
		t.Fatalf("Check = %v, want a create failure", err)
	}
	want := []string{"createdb"}
	if strings.Join(calls, " ") != strings.Join(want, " ") {
		t.Errorf("commands = %v, want %v: a taken name must not be dropped", calls, want)
	}
}

// A drop that fails beside another failure is reported too, never swallowed.
func TestCheckJoinsAFailedDrop(t *testing.T) {
	t.Parallel()
	var calls []string
	r := newFakeRunner(&calls)
	readErr := errors.New("the restored copy holds no issue")
	dropErr := errors.New("exit status 1")
	r.fail["dropdb"] = dropErr
	err := Check(context.Background(), r, config(t, &calls, func(context.Context, string) error { return readErr }))
	if !errors.Is(err, readErr) || !errors.Is(err, dropErr) {
		t.Errorf("Check = %v, want both the read failure and the drop failure", err)
	}
}

// The drop outlives a canceled context: that is when a killed run needs it
// (gt-p85pw.72).
func TestCheckDropsAfterTheContextIsCancelled(t *testing.T) {
	t.Parallel()
	var calls []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newFakeRunner(&calls)
	err := Check(ctx, r, config(t, &calls, func(context.Context, string) error {
		cancel()
		return errors.New("the run was killed")
	}))
	if !strings.Contains(err.Error(), "killed") {
		t.Fatalf("Check = %v, want the read failure", err)
	}
	if got := r.ctxErrs[len(r.ctxErrs)-1]; got != nil {
		t.Errorf("dropdb ran with a canceled context (%v), want a live one", got)
	}
}

// The argv of each command names the dump and the scratch database, and the
// restore connects to the scratch one, not the live one (gt-p85pw.72).
func TestCheckBuildsTheCommands(t *testing.T) {
	t.Parallel()
	var calls []string
	r := newFakeRunner(&calls)
	c := config(t, &calls, nil)
	if err := Check(context.Background(), r, c); err != nil {
		t.Fatalf("Check: %v", err)
	}
	const conninfo = "host=db port=5433 user=tracker dbname=tracker sslmode=require"
	want := [][]string{
		{"/pg/bin/createdb", "--maintenance-db", conninfo, "restorecheck_probe"},
		{"/pg/bin/pg_restore", "--exit-on-error", "--no-owner", "-d", "host=db port=5433 user=tracker sslmode=require dbname=restorecheck_probe", c.Dump},
		{"/pg/bin/dropdb", "--maintenance-db", conninfo, "--if-exists", "--force", "restorecheck_probe"},
	}
	for i, w := range want {
		if strings.Join(r.cmds[i], " ") != strings.Join(w, " ") {
			t.Errorf("command %d = %q, want %q", i, r.cmds[i], w)
		}
	}
}

// A password in the connection string reaches the children in the environment
// only, and every other setting, TLS included, stays in the string they parse
// (gt-p85pw.72).
func TestCheckKeepsThePasswordOutOfArgv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		dsn            string
		wantRestoreDSN string
	}{
		{
			name:           "keyword form",
			dsn:            "host=db user=tracker password='s3cr t' dbname=tracker sslmode=verify-full",
			wantRestoreDSN: "host=db user=tracker sslmode=verify-full dbname=restorecheck_probe",
		},
		{
			name:           "URI form",
			dsn:            "postgres://tracker:s3cret@db.internal:5433/tracker?sslmode=verify-full",
			wantRestoreDSN: "postgres://tracker@db.internal:5433/restorecheck_probe?sslmode=verify-full",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := newFakeRunner(&calls)
			c := config(t, &calls, nil)
			c.DSN = tc.dsn
			if err := Check(context.Background(), r, c); err != nil {
				t.Fatalf("Check: %v", err)
			}
			for _, argv := range r.cmds {
				line := strings.Join(argv, " ")
				if strings.Contains(line, "s3cr") {
					t.Errorf("argv = %q holds the password", argv)
				}
				if !strings.Contains(line, "sslmode=verify-full") {
					t.Errorf("argv = %q lost sslmode=verify-full", argv)
				}
			}
			if got := r.cmds[1][4]; got != tc.wantRestoreDSN {
				t.Errorf("pg_restore -d = %q, want %q", got, tc.wantRestoreDSN)
			}
			for i, env := range r.envs {
				if got := countEnv(env, "PGPASSWORD=s3cr"); got != 1 {
					t.Errorf("command %d: PGPASSWORD settings = %d, want the password once", i, got)
				}
			}
		})
	}
}

// A stale PGPASSWORD is replaced, not duplicated, when the connection string
// carries its own (gt-p85pw.72).
func TestChildEnvReplacesAStalePassword(t *testing.T) {
	t.Parallel()
	env := childEnv([]string{"HOME=/home/tracker", "PGPASSWORD=stale"}, "fresh")
	if got := countEnv(env, "PGPASSWORD=stale"); got != 0 {
		t.Errorf("stale PGPASSWORD kept %d times, want 0", got)
	}
	if got := countEnv(env, "PGPASSWORD=fresh"); got != 1 {
		t.Errorf("PGPASSWORD=fresh appears %d times, want 1", got)
	}
	if got := countEnv(env, "HOME=/home/tracker"); got != 1 {
		t.Errorf("HOME appears %d times, want the rest of the environment kept", got)
	}
}

// A connection string with no password leaves the environment alone, so a
// pass file or an inherited PGPASSWORD still works (gt-p85pw.72).
func TestChildEnvWithNoPasswordKeepsTheEnvironment(t *testing.T) {
	t.Parallel()
	env := childEnv([]string{"PGPASSWORD=inherited"}, "")
	if got := countEnv(env, "PGPASSWORD=inherited"); got != 1 {
		t.Errorf("PGPASSWORD appears %d times, want the inherited one kept", got)
	}
}

// countEnv counts the settings in env that start with prefix.
func countEnv(env []string, prefix string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			n++
		}
	}
	return n
}

// A missing input is refused before any command runs (gt-p85pw.72).
func TestCheckRefusesAMissingInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		edit     func(*Config)
		noRunner bool
		want     string
	}{
		{name: "no connection string", edit: func(c *Config) { c.DSN = "" }, want: "no connection string"},
		{name: "no dump", edit: func(c *Config) { c.Dump = "" }, want: "no dump to restore"},
		{name: "no runner", noRunner: true, want: "no runner"},
		{
			name: "dump that does not exist",
			edit: func(c *Config) { c.Dump = filepath.Join(t.TempDir(), "absent.dump") },
			want: "dump",
		},
		{
			name: "URI that does not parse",
			edit: func(c *Config) { c.DSN = "postgres://tracker:%zz@db.internal/tracker" },
			want: "connection string",
		},
		{
			name: "scratch name that is not a plain identifier",
			edit: func(c *Config) { c.Name = fixedName("Bad-Name") },
			want: "database name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			c := config(t, &calls, nil)
			var r Runner = newFakeRunner(&calls)
			if tc.edit != nil {
				tc.edit(&c)
			}
			if tc.noRunner {
				r = nil
			}
			err := Check(context.Background(), r, c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check = %v, want an error naming %q", err, tc.want)
			}
			if len(calls) != 0 {
				t.Errorf("commands = %v, want none run", calls)
			}
		})
	}
}

// A check with no Name draws a fresh scratch name per run (gt-p85pw.72).
func TestCheckDrawsAFreshScratchName(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		var calls []string
		r := newFakeRunner(&calls)
		c := config(t, &calls, nil)
		c.Name = nil
		if err := Check(context.Background(), r, c); err != nil {
			t.Fatalf("Check: %v", err)
		}
		name := r.cmds[0][len(r.cmds[0])-1]
		if !strings.HasPrefix(name, scratchPrefix) {
			t.Errorf("scratch name = %q, want the %q prefix", name, scratchPrefix)
		}
		if seen[name] {
			t.Errorf("scratch name %q was drawn twice", name)
		}
		seen[name] = true
	}
}
