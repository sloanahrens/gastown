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

// A store-selecting flag is refused while a tracker rig exists, because the
// shim routes by the working directory and would answer from the wrong store
// (gt-p85pw.88).
func TestDecideRefusesAStoreSelectingFlagWhileATrackerExists(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")
	for _, args := range [][]string{
		{"-C", "elsewhere", "list"},
		{"--directory", "elsewhere", "list"},
		{"--directory=elsewhere", "list"},
		{"--db", "x.db", "list"},
		{"--db=x.db", "show", "al-abc"},
		{"list", "-C", "elsewhere"},
	} {
		if _, err := decide(t, alpha, "", args...); err == nil || !strings.Contains(err.Error(), "selects a store") {
			t.Errorf("Decide(%v) from a dolt cwd = %v, want a refusal naming the store-selecting flag", args, err)
		}
	}
	// A value after the separator is an operand, not a flag.
	if _, err := decide(t, alpha, "", "create", "--", "-C"); err != nil {
		t.Errorf("Decide with -C after the separator = %v, want it treated as an operand", err)
	}
}

// With no tracker rig the same flags pass: nothing is routed, bd.real answers.
func TestDecideAllowsAStoreSelectingFlagWithoutATracker(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeGamma)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")
	got, err := decide(t, alpha, "", "-C", "elsewhere", "list")
	if err != nil || got.Tracker {
		t.Fatalf("Decide(-C) with no tracker rig = %+v, %v, want bd.real and no error", got, err)
	}
}

// An operand that opens with a tracker prefix names that rig even when
// IsBeadIDToken rejects its form, so it cannot reach bd.real from a dolt cwd.
func TestDecideRoutesAnIDFormTheTokenCheckRejects(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")
	for _, id := range []string{"bt-abc/def", "bt-abc:1", "bt-abc,bt-def", "bt-ab c"} {
		got, err := decide(t, alpha, "", "show", id)
		if err != nil {
			t.Fatalf("Decide(show %q) = %v", id, err)
		}
		if !got.Tracker || got.Route.Prefix != "bt-" {
			t.Errorf("Decide(show %q) = %+v, want the tracker rig bt-", id, got)
		}
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

// A flag's value that the table did not know read as an id, and an id no route
// names took the working-directory rule away from the command, so a create or
// a list from a tracker rig's directory landed in the rig's stale Dolt store
// (gt-w8j7v). The town's own labels and assignees are hyphenated words, which
// is why a value and an id read alike.
func TestDecideRoutesAFlagValueByTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")
	beta := filepath.Join(town, "beta", "mayor", "rig")

	commands := []struct {
		name string
		args []string
	}{
		{"a short label flag", []string{"create", "T", "-l", "review-deep"}},
		{"a short label flag on list", []string{"list", "-l", "human-only"}},
		{"a short assignee flag", []string{"create", "T", "-a", "town-crew"}},
		{"a long add-label flag", []string{"update", "--add-label", "x-y"}},
	}
	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decide(t, beta, "", tc.args...)
			if err != nil {
				t.Fatalf("Decide(%v) from the tracker rig = %v, want no error", tc.args, err)
			}
			if !got.Tracker || got.Route.Prefix != "bt-" {
				t.Errorf("Decide(%v) from the tracker rig = %+v, want the tracker rig bt-", tc.args, got)
			}

			got, err = decide(t, alpha, "", tc.args...)
			if err != nil {
				t.Fatalf("Decide(%v) from the dolt rig = %v, want no error", tc.args, err)
			}
			if got.Tracker {
				t.Errorf("Decide(%v) from the dolt rig = %+v, want bd.real", tc.args, got)
			}
		})
	}
}

func TestCommandPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a verb", []string{"show", "bt-abc"}, "show"},
		{"a verb with an operand before a flag", []string{"list", "bt-abc", "--json"}, "list"},
		{"a subverb a flag differs on", []string{"gate", "list", "-a"}, "gate list"},
		{"a subverb no flag differs on", []string{"dep", "add", "bt-abc", "bt-def"}, "dep"},
		{"a three word command", []string{"mol", "wisp", "gc"}, "mol wisp gc"},
		{"no args", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := commandPath(tc.args); got != tc.want {
				t.Errorf("commandPath(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// persistentValueFlags are the value flags bd puts on every command, which
// testdata/bd-flags.txt leaves out because every command has them.
var persistentValueFlags = []string{
	"--actor", "--database", "--db", "--directory", "--dolt-auto-commit",
	"--mem-profile", "-C",
}

// bdDocumented reads testdata/bd-flags.txt: the value flags bd documents for
// each command, and the flags it reads as a boolean on one command although
// another command reads a value for the same one.
func bdDocumented(t *testing.T) (value, boolean map[string][]string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "bd-flags.txt"))
	if err != nil {
		t.Fatalf("reading bd's documented flags: %v", err)
	}
	value, boolean = map[string][]string{}, map[string][]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kind, rest, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("testdata/bd-flags.txt: %q names no command", line)
		}
		command, flags, ok := strings.Cut(rest, "\t")
		if !ok || strings.TrimSpace(flags) == "" {
			t.Fatalf("testdata/bd-flags.txt: %q names no flag", line)
		}
		switch kind {
		case "value":
			value[command] = strings.Fields(flags)
		case "bool":
			boolean[command] = strings.Fields(flags)
		default:
			t.Fatalf("testdata/bd-flags.txt: %q has kind %q, want value or bool", line, kind)
		}
	}
	return value, boolean
}

// The tables have to hold what bd documents: a value flag missing from them
// turns its value into an id, and a token read as a value that bd reads as a
// boolean hides the operand behind it. bd's help is pinned in
// testdata/bd-flags.txt rather than run, because a unit test starts no process.
func TestFlagTablesCoverWhatBdDocuments(t *testing.T) {
	t.Parallel()
	value, boolean := bdDocumented(t)

	for command, flags := range value {
		for _, flag := range flags {
			if !takesValue(command, flag) {
				t.Errorf("bd reads the token after %s %s as its value, and the table does not", command, flag)
			}
		}
	}
	for command, flags := range boolean {
		for _, flag := range flags {
			if takesValue(command, flag) {
				t.Errorf("bd reads %s %s as a boolean, and the table reads the token after it as its value", command, flag)
			}
		}
	}

	// The other way: a flag in the table that bd documents nowhere is a typo,
	// or a flag bd dropped.
	documented := map[string]bool{}
	for _, flags := range value {
		for _, flag := range flags {
			documented[flag] = true
		}
	}
	for _, flag := range persistentValueFlags {
		documented[flag] = true
	}
	for flag := range valueFlags {
		if !documented[flag] {
			t.Errorf("%s is in valueFlags and bd documents no value for it", flag)
		}
	}

	// A flag the table calls a boolean on a command is one bd calls a boolean
	// there, and the file names every such command.
	named := map[string]bool{}
	for command, flags := range boolean {
		for _, flag := range flags {
			named[command+" "+flag] = true
		}
	}
	for command, flags := range boolFlags {
		for flag := range flags {
			if !named[command+" "+flag] {
				t.Errorf("boolFlags calls %s %s a boolean, and bd's help does not", command, flag)
			}
			delete(named, command+" "+flag)
		}
	}
	for name := range named {
		t.Errorf("bd reads %s as a boolean on one command, and boolFlags does not say so", name)
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
		{"a short label value", []string{"create", "T", "-l", "review-deep"}, nil},
		{"a short assignee value", []string{"list", "-a", "bt-abc"}, nil},
		{"-f is a value flag on create", []string{"create", "T", "-f", "notes.md", "bt-abc"}, []string{"bt-abc"}},
		{"-f is a boolean on close", []string{"close", "-f", "bt-abc"}, []string{"bt-abc"}},
		{"-r is a value flag on close", []string{"close", "-r", "bt-abc"}, nil},
		{"-r is a boolean on list", []string{"list", "-r", "bt-abc"}, []string{"bt-abc"}},
		{"-a is a boolean on gate list", []string{"gate", "list", "-a", "bt-abc"}, []string{"bt-abc"}},
		{"a bare --deps takes no value on list", []string{"list", "--deps", "bt-abc"}, []string{"bt-abc"}},
		{"--deps takes a value on create", []string{"create", "T", "--deps", "bt-abc"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := idTokens(commandPath(tc.args), tc.args)
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
