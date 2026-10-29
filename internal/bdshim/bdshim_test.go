package bdshim

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// The route table every routing test starts from: a town-level store, a Dolt
// rig, a tracker rig and a rig whose backend is written out.
const (
	routeTown  = `{"path":".","prefix":"hq-"}`
	routeAlpha = `{"path":"alpha/mayor/rig","prefix":"al-"}`
	routeBeta  = `{"path":"beta/mayor/rig","prefix":"bt-","backend":"tracker"}`
	routeGamma = `{"path":"gamma/mayor/rig","prefix":"gm-","backend":"dolt"}`
)

// newTown writes a town whose route table is routes and creates each store the
// table names, so a rig directory is a real place a command can stand in.
func newTown(t *testing.T, routes ...string) string {
	t.Helper()
	town := t.TempDir()
	writeFile(t, filepath.Join(town, ".beads", beads.RoutesFileName), strings.Join(routes, "\n")+"\n")
	for _, line := range routes {
		var route beads.Route
		if strings.HasPrefix(line, "#") || line == "" {
			continue // a comment or a blank line, which the table allows
		}
		if err := json.Unmarshal([]byte(line), &route); err != nil {
			t.Fatalf("fixture route %s: %v", line, err)
		}
		if route.Path == "." {
			continue
		}
		mkdir(t, filepath.Join(town, route.Path, ".beads"))
	}
	return town
}

// writeFile writes body at path, creating the directories above it.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// mkdir creates dir and its parents.
func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
}

// beadsDirOnly is the environment a test gives: BEADS_DIR set to beadsDir and
// no other variable set, so a lookup of any other name finds nothing.
func beadsDirOnly(beadsDir string) func(string) string {
	return func(name string) string {
		if name == beadsDirEnv {
			return beadsDir
		}
		return ""
	}
}

// decide runs Decide with a fixed working directory and BEADS_DIR.
func decide(t *testing.T, cwd, beadsDir string, args ...string) (Decision, error) {
	t.Helper()
	return Decide(beadsDirOnly(beadsDir), func() (string, error) { return cwd, nil }, args)
}

func TestDecideRoutesByStore(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta, routeGamma)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")
	beta := filepath.Join(town, "beta", "mayor", "rig")
	// A polecat worktree holds a redirect to its rig's store, as the town's
	// own worktrees do.
	worktree := filepath.Join(town, "beta", "polecats", "goose", "checkout")
	writeFile(t, filepath.Join(worktree, ".beads", "redirect"), "../../../mayor/rig/.beads\n")

	cases := []struct {
		name        string
		args        []string
		cwd         string
		beadsDir    string
		wantTracker bool
		wantPrefix  string
	}{
		{"an id names its tracker rig", []string{"show", "bt-abc"}, town, "", true, "bt-"},
		{"an id names its dolt rig from a tracker cwd", []string{"show", "al-abc"}, beta, "", false, ""},
		{"an unknown prefix reaches bd.real from a tracker cwd", []string{"show", "zz-abc"}, beta, "", false, ""},
		{"a command with no id and no rig is dolt", []string{"list"}, town, "", false, ""},
		{"a command with no id takes the rig of its cwd", []string{"list"}, beta, "", true, "bt-"},
		{"a worktree redirect names its rig", []string{"ready"}, worktree, "", true, "bt-"},
		{"BEADS_DIR names the rig", []string{"list"}, town, filepath.Join(beta, ".beads"), true, "bt-"},
		{"a relative BEADS_DIR resolves against the cwd", []string{"list"}, alpha, "../../../beta/mayor/rig/.beads", true, "bt-"},
		{"BEADS_DIR of a dolt rig is dolt", []string{"list"}, beta, filepath.Join(alpha, ".beads"), false, ""},
		{"a flag value is not an id", []string{"create", "--title", "bt-fix"}, alpha, "", false, ""},
		{"a parent value does not route its own create", []string{"create", "--parent", "bt-abc"}, alpha, "", false, ""},
		{"an inline flag value is not an id", []string{"list", "--format=bt-x"}, town, "", false, ""},
		{"an id after a flag still names its rig", []string{"show", "bt-abc", "--json"}, alpha, "", true, "bt-"},
		{"an id after a boolean flag names its tracker rig", []string{"show", "--json", "bt-abc"}, alpha, "", true, "bt-"},
		{"an id after a value flag names its tracker rig", []string{"create", "--title", "x", "bt-abc"}, alpha, "", true, "bt-"},
		{"two ids of one tracker rig name it", []string{"dep", "add", "bt-abc", "bt-def"}, town, "", true, "bt-"},
		{"an explicit dolt backend is dolt", []string{"show", "gm-abc"}, beta, "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decide(t, tc.cwd, tc.beadsDir, tc.args...)
			if err != nil {
				t.Fatalf("Decide(%v) = %v, want no error", tc.args, err)
			}
			if got.Tracker != tc.wantTracker {
				t.Errorf("Decide(%v).Tracker = %v, want %v", tc.args, got.Tracker, tc.wantTracker)
			}
			if got.Route.Prefix != tc.wantPrefix {
				t.Errorf("Decide(%v).Route.Prefix = %q, want %q", tc.args, got.Route.Prefix, tc.wantPrefix)
			}
		})
	}
}

func TestDecideReadsTheTownsOwnRouteTable(t *testing.T) {
	t.Parallel()
	// beads.LoadRoutes keeps the shape a town writes: comments and blank lines
	// between routes.
	town := newTown(t, "# rig routes", "", routeTown, routeBeta)

	got, err := decide(t, filepath.Join(town, "beta", "mayor", "rig"), "", "list")
	if err != nil {
		t.Fatalf("Decide = %v, want no error", err)
	}
	if !got.Tracker {
		t.Error("Decide with a commented route table = dolt, want the tracker rig")
	}
}

func TestDecideRefusesIDsSpanningStores(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)

	_, err := decide(t, town, "", "show", "al-abc", "bt-def")
	if err == nil {
		t.Fatal("Decide across a dolt rig and a tracker rig = nil error, want a refusal")
	}
}

func TestDecideWithoutAWorkingDirectory(t *testing.T) {
	t.Parallel()
	getwd := func() (string, error) { return "", errors.New("no working directory") }

	if _, err := Decide(func(string) string { return "" }, getwd, []string{"list"}); err == nil {
		t.Fatal("Decide with no working directory = nil error, want a refusal")
	}
}

func TestDecideWithoutRouteTableIsDolt(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()

	got, err := decide(t, empty, "", "show", "bt-abc")
	if err != nil {
		t.Fatalf("Decide with no route table = %v, want a pass-through", err)
	}
	if got.Tracker {
		t.Error("Decide with no route table = tracker, want bd.real")
	}
}

func TestDecideFailsClosedOnMalformedRouteTable(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown)
	unreadable := filepath.Join(town, "unreadable")
	mkdir(t, filepath.Join(unreadable, ".beads", beads.RoutesFileName)) // a directory where the table belongs

	unknownBackend := newTown(t, routeTown, `{"path":"beta/mayor/rig","prefix":"bt-","backend":"postgres"}`)

	// A working directory that runs through a file, as a typo in BEADS_DIR can.
	throughFile := newTown(t, routeTown)
	writeFile(t, filepath.Join(throughFile, "notadir"), "x")

	// A .beads that is a file where a store directory belongs.
	fileStore := newTown(t, routeTown)
	writeFile(t, filepath.Join(fileStore, "rig", ".beads"), "x")

	cases := []struct {
		name string
		cwd  string
	}{
		{"an unreadable table", unreadable},
		{"a backend the shim cannot classify", unknownBackend},
		{"a working directory the walk cannot read", filepath.Join(throughFile, "notadir", "below")},
		{"a .beads entry that is a file", filepath.Join(fileStore, "rig")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decide(t, tc.cwd, "", "show", "bt-abc"); err == nil {
				t.Fatal("Decide = nil error, want a refusal")
			}
		})
	}
}

func TestDecideVerbs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a verb after no flag", []string{"show", "bt-abc"}, "show"},
		{"a verb after a boolean flag", []string{"--json", "list"}, "list"},
		{"a verb after a global with a value", []string{"--db", "hq", "list"}, "list"},
		{"a verb after a global with an inline value", []string{"--db=hq", "list"}, "list"},
		{"a verb after a value flag with an operand", []string{"--db", "hq", "--json", "list"}, "list"},
		{"no args", nil, ""},
		{"past a separator", []string{"--", "show"}, "show"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := verbOf(tc.args); got != tc.want {
				t.Errorf("verbOf(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestIDTokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"an id", []string{"show", "bt-abc"}, []string{"bt-abc"}},
		{"an id before a flag", []string{"show", "bt-abc", "--json"}, []string{"bt-abc"}},
		{"a flag value", []string{"create", "--title", "bt-fix"}, nil},
		{"an inline flag value", []string{"show", "--id=bt-abc"}, nil},
		{"an id after a boolean flag", []string{"show", "--json", "bt-abc"}, []string{"bt-abc"}},
		{"an id after a value flag", []string{"create", "--title", "x", "bt-abc"}, []string{"bt-abc"}},
		{"an id past a separator", []string{"show", "--", "bt-abc"}, []string{"bt-abc"}},
		{"a flag-like token past a separator names no bead", []string{"show", "--", "--json"}, nil},
		{"two ids", []string{"dep", "add", "bt-abc", "bt-def"}, []string{"bt-abc", "bt-def"}},
		{"a verb", []string{"list"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := idTokens(tc.args)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("idTokens(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		verb, prefix, want string
	}{
		{"show", "bt-", "bdshim: verb show is not implemented for tracker rig bt-"},
		{"", "bt-", "bdshim: verb (none) is not implemented for tracker rig bt-"},
	}
	for _, tc := range cases {
		t.Run(tc.verb+"on"+tc.prefix, func(t *testing.T) {
			t.Parallel()
			if got := Refusal(tc.verb, tc.prefix); got != tc.want {
				t.Errorf("Refusal(%q, %q) = %q, want %q", tc.verb, tc.prefix, got, tc.want)
			}
		})
	}
}

// fakeChild is a bd.real status, shaped as exec.ExitError is.
type fakeChild int

func (f fakeChild) Error() string { return "bd.real ended" }

func (f fakeChild) ExitCode() int { return int(f) }

// fakeProcess is the process surface a test hands Main, recording what the
// shim asked it to run.
type fakeProcess struct {
	ran  []string // the argv Main ran, empty when it ran nothing
	path string
	err  error
}

func (p *fakeProcess) run(path string, argv []string) error {
	p.path, p.ran = path, argv
	return p.err
}

// shimEnv drives Main with cwd, BEADS_DIR and a child process of the test's
// choosing; the shim's own path sits in a bin directory of its own.
func shimEnv(t *testing.T, cwd, beadsDir string, proc *fakeProcess, handlers map[string]Handler, args ...string) (Env, *bytes.Buffer, string) {
	t.Helper()
	stderr := &bytes.Buffer{}
	shimPath := filepath.Join(t.TempDir(), "bin", "bdshim")
	return Env{
		Args:       args,
		Getenv:     beadsDirOnly(beadsDir),
		Getwd:      func() (string, error) { return cwd, nil },
		Executable: func() (string, error) { return shimPath, nil },
		Run:        proc.run,
		Handlers:   handlers,
		Stderr:     stderr,
	}, stderr, shimPath
}

func TestMainPassesADoltCommandToRealBd(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	args := []string{"show", "al-abc", "--json"}

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"bd's own exit status", fakeChild(13), 13},
		{"bd's success", nil, 0},
		{"bd.real that never started", errors.New("fork/exec: no such file"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			proc := &fakeProcess{err: tc.err}
			e, stderr, shimPath := shimEnv(t, town, "", proc, nil, args...)
			if got := Main(e); got != tc.want {
				t.Errorf("Main = %d, want %d (stderr %q)", got, tc.want, stderr.String())
			}
			if strings.Join(proc.ran, " ") != strings.Join(args, " ") {
				t.Errorf("Main ran %v, want bd's argv %v unchanged", proc.ran, args)
			}
			if want := filepath.Join(filepath.Dir(shimPath), realName); proc.path != want {
				t.Errorf("Main ran %q, want bd.real beside the shim at %q", proc.path, want)
			}
			if tc.want == 1 && stderr.Len() == 0 {
				t.Error("Main failed without a message on stderr")
			}
		})
	}
}

func TestMainRefusesATrackerVerb(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)

	cases := []struct {
		name string
		args []string
		cwd  string
		want string
	}{
		{"an id names the tracker rig", []string{"show", "bt-abc"}, town, "bdshim: verb show is not implemented for tracker rig bt-\n"},
		{"an id after a boolean flag names the tracker rig", []string{"show", "--json", "bt-abc"}, town, "bdshim: verb show is not implemented for tracker rig bt-\n"},
		{"a verb with no id takes its cwd's rig", []string{"create", "--title", "x"}, filepath.Join(town, "beta", "mayor", "rig"), "bdshim: verb create is not implemented for tracker rig bt-\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			proc := &fakeProcess{}
			e, stderr, _ := shimEnv(t, tc.cwd, "", proc, nil, tc.args...)
			if got := Main(e); got != 1 {
				t.Errorf("Main = %d, want 1", got)
			}
			if stderr.String() != tc.want {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.want)
			}
			if proc.ran != nil {
				t.Errorf("Main ran %v, want no fallback to bd.real", proc.ran)
			}
		})
	}
}

func TestMainAnswersWithARegisteredHandler(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeBeta)
	proc := &fakeProcess{}
	answered := 0
	handlers := map[string]Handler{
		"show": func(Env, Decision) int { answered++; return 7 },
	}
	e, _, _ := shimEnv(t, town, "", proc, handlers, "show", "bt-abc")

	if got := Main(e); got != 7 {
		t.Errorf("Main = %d, want the handler's 7", got)
	}
	if answered != 1 {
		t.Errorf("the handler answered %d times, want once", answered)
	}
	if proc.ran != nil {
		t.Errorf("Main ran %v, want the handler to answer instead of bd.real", proc.ran)
	}
}

func TestMainFailsClosedOnAMalformedRouteTable(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown)
	mkdir(t, filepath.Join(town, "unreadable", ".beads", beads.RoutesFileName))
	proc := &fakeProcess{}
	e, stderr, _ := shimEnv(t, filepath.Join(town, "unreadable"), "", proc, nil, "show", "bt-abc")

	if got := Main(e); got != 1 {
		t.Errorf("Main = %d, want 1", got)
	}
	if proc.ran != nil {
		t.Errorf("Main ran %v, want nothing to run on a malformed table", proc.ran)
	}
	if !strings.HasPrefix(stderr.String(), "bdshim: ") {
		t.Errorf("stderr = %q, want a bdshim message", stderr.String())
	}
}
