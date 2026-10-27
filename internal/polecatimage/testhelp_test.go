package polecatimage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testNow is the clock every test run starts at, so a lock age and a log line
// are one exact value.
var testNow = time.Date(2026, 10, 26, 19, 20, 36, 0, time.UTC)

// testRun is one run rooted in a temporary town: a case answers the program
// calls it names, and every call is recorded whether it answered or not.
type testRun struct {
	*Run
	root   string
	logged strings.Builder
	calls  []command
	now    time.Time
	reply  func(command) result
}

func newTestRun(t *testing.T) *testRun {
	t.Helper()
	root := t.TempDir()
	tr := &testRun{root: root, now: testNow}
	tr.Run = &Run{
		Cfg: Config{
			GT:             "gt",
			Docker:         "docker",
			Git:            "git",
			Make:           "make",
			Pgrep:          "pgrep",
			Openshell:      "openshell",
			RigRepo:        filepath.Join(root, "rig.git"),
			BuildRoot:      filepath.Join(root, "build"),
			BDSrc:          filepath.Join(root, "beads"),
			LockDir:        filepath.Join(root, "runtime", "polecat-image.lock"),
			StaleAfter:     20 * time.Minute,
			SandboxRecords: filepath.Join(root, "runtime", "sandboxes"),
			Image:          "gastown-polecat:dev",
			Label:          "org.gastown.gt-commit",
			PID:            4242,
		},
		run: tr.record,
		now: func() time.Time { return tr.now },
		out: &tr.logged,
	}
	return tr
}

// record answers one call through the case's reply and keeps the call either
// way, so a test can assert what a failing run never ran.
func (tr *testRun) record(c command) result {
	tr.calls = append(tr.calls, c)
	if tr.reply == nil {
		return result{}
	}
	return tr.reply(c)
}

// output is everything the run logged.
func (tr *testRun) output() string { return tr.logged.String() }

// callsTo is the calls to the program name, in order.
func (tr *testRun) callsTo(name string) []command {
	var out []command
	for _, c := range tr.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// argsOf is the first call's arguments, and whether the program was called.
func (tr *testRun) argsOf(name string) ([]string, bool) {
	calls := tr.callsTo(name)
	if len(calls) == 0 {
		return nil, false
	}
	return calls[0].args, true
}

// wantArgs fails unless the first call to name carried these arguments.
func (tr *testRun) wantArgs(t *testing.T, name string, want ...string) {
	t.Helper()
	got, ok := tr.argsOf(name)
	if !ok {
		t.Fatalf("%s was never called", name)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("%s called with %q, want %q", name, got, want)
	}
}

// wantLogged fails unless the run logged want as a substring.
func (tr *testRun) wantLogged(t *testing.T, want string) {
	t.Helper()
	if !strings.Contains(tr.output(), want) {
		t.Errorf("log %q does not contain %q", tr.output(), want)
	}
}

// makeLock plants a lock directory with pid in it, last modified at when.
func makeLock(t *testing.T, dir, pid string, when time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if pid != "" {
		if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(pid+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
}

// lockPID is the pid the lock records, "" when it records none.
func lockPID(t *testing.T, dir string) string {
	t.Helper()
	pid, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(pid))
}

// asideDirs is the takeover directories a run left beside its lock, which a
// takeover must never leave behind.
func asideDirs(t *testing.T, lockDir string) []string {
	t.Helper()
	left, err := filepath.Glob(lockDir + ".stale.*")
	if err != nil {
		t.Fatal(err)
	}
	return left
}
