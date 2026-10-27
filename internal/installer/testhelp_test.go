package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testNow is the clock every test run uses, so a marker or a receipt is one
// exact line.
var testNow = time.Date(2026, 10, 26, 19, 20, 36, 0, time.UTC)

// testRun is a run whose seams answer as a clean checkout on main at head
// "abc123", with nothing installed, the real filesystem under temporary
// directories, and escalations recorded instead of run. A case sets only the
// seam it is about (gt-tpg0p.10).
type testRun struct {
	*Run
	out    strings.Builder
	errOut strings.Builder
	execs  [][]string
	// cmds is the argv of every command the install sections ran, through Cmd,
	// dirs the directory each ran in, and stdins the standard input each was
	// fed.
	cmds   [][]string
	dirs   []string
	stdins []string
	// respond answers a command; a case that leaves it nil gets status 0 and
	// no output.
	respond func(dir, name string, args []string) (stdout, stderr []byte, code int)
}

func newTestRun(t *testing.T) *testRun {
	t.Helper()
	root := t.TempDir()
	paths := Paths{
		BinDir:    filepath.Join(root, "bin"),
		DaemonDir: filepath.Join(root, "daemon"),
		RigDir:    filepath.Join(root, "rig"),
		Home:      filepath.Join(root, "home"),
	}
	for _, dir := range []string{paths.BinDir, paths.DaemonDir, paths.RigDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tr := &testRun{}
	tr.Run = &Run{
		Cfg:   Config{SHA: "origin/main", Source: "manual", SlotTimeout: "600", ImageFirst: true},
		Paths: paths,
		C: &Contract{
			Stdout:   &tr.out,
			Stderr:   &tr.errOut,
			Expected: "origin/main",
			Source:   "manual",
			Start:    testNow.Add(-5 * time.Second),
			Now:      func() time.Time { return testNow },
			GT:       paths.GT(),
			Exec: func(name string, args ...string) ([]byte, error) {
				tr.execs = append(tr.execs, append([]string{name}, args...))
				return nil, nil
			},
		},
		Repo: Repo{
			Fetch:      func() error { return nil },
			Resolve:    func(ref string) string { return ref },
			CommitTime: func(string) (time.Time, error) { return testNow, nil },
			IsAncestor: func(string, string) bool { return false },
			Dirty:      func() (bool, error) { return false, nil },
			Branch:     func() string { return "main" },
			FastForward: func(string) error {
				return nil
			},
			Head:    func() (string, error) { return "abc123", nil },
			AheadOf: func(string) (int, error) { return 0, nil },
		},
		Files: Files{Exists: func(string) bool { return true }},
		Bin:   Binary{Commit: func(string, string) string { return "" }},
		// Signing material under the temporary root, so a run that reaches the
		// signing step reads nothing of the host's (gt-tpg0p.11).
		Signing: Signing{Dir: filepath.Join(root, "signing"), Identity: "gastown-dev-unattended", Timeout: "30"},
		Found:   func(string) bool { return false },
	}
	tr.Cmd = func(dir, name string, stdin []byte, args ...string) ([]byte, []byte, int) {
		tr.cmds = append(tr.cmds, append([]string{name}, args...))
		tr.dirs = append(tr.dirs, dir)
		tr.stdins = append(tr.stdins, string(stdin))
		if tr.respond != nil {
			return tr.respond(dir, name, args)
		}
		return nil, nil, 0
	}
	return tr
}

// containsLine reports whether lines holds want.
func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// command is the argv of the nth command the run made, 0 being the first.
func (tr *testRun) command(n int) []string {
	if n < 0 || n >= len(tr.cmds) {
		return nil
	}
	return tr.cmds[n]
}

// resolved is the state ResolveTarget leaves, with prev as the installed
// commit, so a section after it can be tested on its own.
func (tr *testRun) resolved(prev string) {
	tr.Target = Target{FullSHA: "c2222222", Prev: prev}
	tr.C.Expected = "c2222222"
	tr.C.Prev = prev
}

// lines is stdout split into the lines a reader sees.
func (tr *testRun) lines() []string {
	trimmed := strings.TrimSuffix(tr.out.String(), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// cmd is the argv of the last escalation, which the fake Exec recorded.
func (tr *testRun) escalation() []string {
	if len(tr.execs) == 0 {
		return nil
	}
	return tr.execs[len(tr.execs)-1]
}

// write puts name in the daemon directory with body, as the daemon would.
func (tr *testRun) write(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tr.Paths.DaemonDir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ancestors is an IsAncestor answer for exactly the pairs given.
func ancestors(pairs ...[2]string) func(string, string) bool {
	return func(ancestor, descendant string) bool {
		for _, p := range pairs {
			if p[0] == ancestor && p[1] == descendant {
				return true
			}
		}
		return false
	}
}
