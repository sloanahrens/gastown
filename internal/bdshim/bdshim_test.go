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
		{"an id after a value flag names its tracker rig", []string{"update", "--title", "x", "bt-abc"}, alpha, "", true, "bt-"},
		{"a title names no id, so the cwd's tracker rig answers", []string{"create", "refactor-sling"}, beta, "", true, "bt-"},
		{"a title that opens with a tracker prefix is not an id", []string{"create", "bt-fix-thing"}, alpha, "", false, ""},
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

// A flag whose meaning on the command is an issue id names that bead, so the
// command reaches the store that holds it even from another rig's directory:
// show --id bt-abc and dep add --depends-on bt-def routed by the working
// directory and answered not-found for a bead the tracker holds (gt-b00kz).
func TestDecideRoutesABeadValueFlagToItsStore(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")

	cases := []struct {
		name string
		args []string
	}{
		{"show --id", []string{"show", "--id", "bt-abc"}},
		{"show with an inline --id", []string{"show", "--id=bt-abc"}},
		{"list --id", []string{"list", "--id", "bt-abc"}},
		{"list --id of two ids", []string{"list", "--id", "bt-abc,bt-def"}},
		{"list --parent", []string{"list", "--parent", "bt-abc"}},
		{"count --id", []string{"count", "--id", "bt-abc"}},
		{"blocked --parent", []string{"blocked", "--parent", "bt-abc"}},
		{"ready --parent", []string{"ready", "--parent", "bt-abc"}},
		{"reclaim --id", []string{"reclaim", "--id", "bt-abc"}},
		{"dep --blocks", []string{"dep", "bt-def", "--blocks", "bt-abc"}},
		{"dep with a short --blocks", []string{"dep", "bt-def", "-b", "bt-abc"}},
		{"dep add --depends-on", []string{"dep", "add", "bt-def", "--depends-on", "bt-abc"}},
		{"dep add --blocked-by", []string{"dep", "add", "--blocked-by", "bt-abc", "bt-def"}},
		{"duplicate --of", []string{"duplicate", "bt-abc", "--of", "bt-def"}},
		{"supersede --with", []string{"supersede", "bt-abc", "--with", "bt-def"}},
		{"provenance record --issue", []string{"provenance", "record", "--issue", "bt-abc"}},
		{"update --parent", []string{"update", "bt-def", "--parent", "bt-abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decide(t, alpha, "", tc.args...)
			if err != nil {
				t.Fatalf("Decide(%v) from the dolt rig = %v, want no error", tc.args, err)
			}
			if !got.Tracker || got.Route.Prefix != "bt-" {
				t.Errorf("Decide(%v) from the dolt rig = %+v, want the tracker rig bt-", tc.args, got)
			}
		})
	}
}

// A command that writes its bead in the store its working directory names is
// not routed by the issues its flags point at: create, create-form, q and gate
// create set the parent, dependency or block of the bead they write, and a run
// id in --await-id names no bead.
func TestDecideWritesInTheWorkingDirectorysStore(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")

	cases := []struct {
		name string
		args []string
	}{
		{"create --parent", []string{"create", "T", "--parent", "bt-abc"}},
		{"create with an inline --parent", []string{"create", "T", "--parent=bt-abc"}},
		{"create --waits-for", []string{"create", "T", "--waits-for", "bt-abc"}},
		{"create-form --parent", []string{"create-form", "--parent", "bt-abc"}},
		{"q --parent", []string{"q", "T", "--parent", "bt-abc"}},
		{"gate create --await-id", []string{"gate", "create", "--await-id", "bt-abc"}},
		{"gate create --blocks", []string{"gate", "create", "--blocks", "bt-abc"}},
		{"update --await-id", []string{"update", "al-abc", "--await-id", "bt-abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decide(t, alpha, "", tc.args...)
			if err != nil {
				t.Fatalf("Decide(%v) from the dolt rig = %v, want no error", tc.args, err)
			}
			if got.Tracker {
				t.Errorf("Decide(%v) from the dolt rig = %+v, want bd.real", tc.args, got)
			}
		})
	}
}

// A bead-naming flag value that no route names takes the working-directory
// rule away from the command, as an operand does: list --id zz-abc from a
// tracker rig's directory must not list the tracker's beads.
func TestDecideRoutesAnUnknownFlagValueByTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	beta := filepath.Join(town, "beta", "mayor", "rig")

	for _, args := range [][]string{
		{"list", "--id", "zz-abc"},
		{"show", "--id", "gm-abc"},
	} {
		got, err := decide(t, beta, "", args...)
		if err != nil {
			t.Fatalf("Decide(%v) = %v, want no error", args, err)
		}
		if got.Tracker {
			t.Errorf("Decide(%v) from the tracker rig = %+v, want bd.real", args, got)
		}
	}
}

// An operand and a flag value that name different stores, one of them a
// tracker, cannot be one command's target, so it is refused rather than routed
// to the rig the operand names.
func TestDecideRefusesAFlagValueSpanningStores(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")

	_, err := decide(t, alpha, "", "dep", "add", "al-abc", "--depends-on", "bt-def")
	if err == nil {
		t.Fatal("Decide(dep add al-abc --depends-on bt-def) = nil error, want the cross-store refusal")
	}
	if !strings.Contains(err.Error(), "stores") {
		t.Errorf("Decide(dep add al-abc --depends-on bt-def) = %v, want the cross-store refusal", err)
	}
}

// A flag bd reads as a value on one command is not one it reads a value for on
// another: -f is --file on create and --force on close. Reading it by the flat
// flag name swallowed the id behind it, so `bd close -f bt-abc` from a Dolt
// directory reached bd.real instead of the tracker rig that holds bt-abc.
func TestDecideReadsAFlagsMeaningPerCommand(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")

	for _, args := range [][]string{
		{"close", "-f", "bt-abc"},
		{"list", "-r", "bt-abc"},
		{"gate", "list", "-a", "bt-abc"},
	} {
		got, err := decide(t, alpha, "", args...)
		if err != nil {
			t.Fatalf("Decide(%v) = %v, want no error", args, err)
		}
		if !got.Tracker || got.Route.Prefix != "bt-" {
			t.Errorf("Decide(%v) = %+v, want the tracker rig bt-", args, got)
		}
	}
}

// bd reads a word as its command's alias, `bd new -l review-deep` being
// `bd create -l review-deep`: the value stays a value, so the command routes by
// the directory it runs in rather than by review-deep's prefix.
func TestDecideReadsAnAliasAsItsCommand(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")
	beta := filepath.Join(town, "beta", "mayor", "rig")

	got, err := decide(t, beta, "", "new", "-l", "review-deep")
	if err != nil {
		t.Fatalf("Decide(new -l review-deep) from the tracker rig = %v, want no error", err)
	}
	if !got.Tracker || got.Route.Prefix != "bt-" {
		t.Errorf("Decide(new -l review-deep) from the tracker rig = %+v, want the tracker rig bt-", got)
	}
	got, err = decide(t, alpha, "", "new", "-l", "review-deep")
	if err != nil {
		t.Fatalf("Decide(new -l review-deep) from the dolt rig = %v, want no error", err)
	}
	if got.Tracker {
		t.Errorf("Decide(new -l review-deep) from the dolt rig = %+v, want bd.real", got)
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
		{"a subverb with its own value flags", []string{"dep", "add", "bt-abc", "bt-def"}, "dep add"},
		{"an alias of a verb", []string{"new", "T"}, "create"},
		{"an alias of a subverb", []string{"backup", "add"}, "backup init"},
		{"a subverb under an alias", []string{"protomolecule", "wisp", "gc"}, "mol wisp gc"},
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

// bdDocumented reads testdata/bd-flags.txt, the flags bd 1.2.2 documents, so
// the tables the shim decides with can be checked against bd itself. The file
// records bd's help rather than running it, because a unit test starts no
// process; refresh it with `go run ./cmd/bdshim-record -flags`.
func bdDocumented(t *testing.T) (value, boolean map[string]map[string]bool, persistent map[string]bool, aliased map[string]string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "bd-flags.txt"))
	if err != nil {
		t.Fatalf("reading the recorded bd flags: %v", err)
	}
	value, boolean = map[string]map[string]bool{}, map[string]map[string]bool{}
	persistent = map[string]bool{}
	aliased = map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kind, rest, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("testdata/bd-flags.txt: %q names no command", line)
		}
		if kind == "persistent" {
			for _, flag := range strings.Fields(rest) {
				persistent[flag] = true
			}
			continue
		}
		if kind == "alias" {
			name, command, ok := strings.Cut(rest, "\t")
			if !ok || command == "" {
				t.Fatalf("testdata/bd-flags.txt: %q names no command its alias stands for", line)
			}
			aliased[name] = command
			continue
		}
		if kind == "operand" {
			continue // bdOperands reads these
		}
		command, flags, ok := strings.Cut(rest, "\t")
		if !ok || strings.TrimSpace(flags) == "" {
			t.Fatalf("testdata/bd-flags.txt: %q names no flag", line)
		}
		row := map[string]bool{}
		for _, flag := range strings.Fields(flags) {
			row[flag] = true
		}
		switch kind {
		case "value":
			value[command] = row
		case "bool":
			boolean[command] = row
		default:
			t.Fatalf("testdata/bd-flags.txt: %q has kind %q, want persistent, alias, operand, value or bool", line, kind)
		}
	}
	return value, boolean, persistent, aliased
}

// bdOperands reads the operand rows of testdata/bd-flags.txt: the positional
// placeholders bd's usage line names for each command, in order.
func bdOperands(t *testing.T) map[string][]string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "bd-flags.txt"))
	if err != nil {
		t.Fatalf("reading the recorded bd flags: %v", err)
	}
	operand := map[string][]string{}
	for _, line := range strings.Split(string(body), "\n") {
		kind, rest, ok := strings.Cut(line, "\t")
		if !ok || kind != "operand" {
			continue
		}
		command, placeholders, ok := strings.Cut(rest, "\t")
		if !ok || strings.TrimSpace(placeholders) == "" {
			t.Fatalf("testdata/bd-flags.txt: %q names no placeholder", line)
		}
		operand[command] = strings.Fields(placeholders)
	}
	return operand
}

// TestOperandTableCoversWhatBdDocuments checks operandSlots against the recorded
// usage lines both ways: a command bd documents with a positional placeholder
// that the table lacks reads its operands with no slot, and a row the table
// holds for a placeholder bd does not write is a typo or a moved argument.
func TestOperandTableCoversWhatBdDocuments(t *testing.T) {
	t.Parallel()
	documented := bdOperands(t)
	// The check below walks a map, so a recording that parsed to nothing would
	// pass it and leave the table looking complete.
	if len(documented) < 100 {
		t.Fatalf("testdata/bd-flags.txt documents positionals for %d commands, want at least 100 (bd 1.2.2 documents 112)", len(documented))
	}
	for command, placeholders := range documented {
		slots, ok := operandSlots[command]
		if !ok {
			t.Errorf("bd documents the positional %v for %q, and operandSlots has no row for it", placeholders, command)
			continue
		}
		var got []string
		for _, slot := range slots {
			got = append(got, slot.placeholder)
		}
		if strings.Join(got, " ") != strings.Join(placeholders, " ") {
			t.Errorf("operandSlots for %q = %v, want the placeholders bd writes %v", command, got, placeholders)
		}
	}
	for command := range operandSlots {
		if documented[command] == nil {
			t.Errorf("operandSlots has a row for %q, and bd documents no positional for it", command)
		}
	}
}

// TestFlagTablesCoverWhatBdDocuments checks the shim's tables against the
// recorded bd help, per command and both ways: a value flag bd documents that
// the table lacks turns its value into an id, and a flag the table reads a
// value for that bd reads as a boolean hides the operand behind it.
func TestFlagTablesCoverWhatBdDocuments(t *testing.T) {
	t.Parallel()
	value, boolean, persistent, aliased := bdDocumented(t)

	// Every check below walks a map, so a recording that parsed to nothing
	// would pass them all and leave the tables looking complete.
	if len(value) < 50 {
		t.Fatalf("testdata/bd-flags.txt documents a value flag for %d commands, want bd 1.2.2's 100", len(value))
	}
	if len(boolean) < 10 {
		t.Fatalf("testdata/bd-flags.txt documents %d commands with a boolean conflict, want bd 1.2.2's 20", len(boolean))
	}
	if len(persistent) < 5 {
		t.Fatalf("testdata/bd-flags.txt documents %d persistent value flags, want bd 1.2.2's 6", len(persistent))
	}
	if len(aliased) < 5 {
		t.Fatalf("testdata/bd-flags.txt documents %d command aliases, want bd 1.2.2's 12", len(aliased))
	}

	for command, flags := range value {
		for flag := range flags {
			if !takesValue(command, flag) {
				t.Errorf("bd reads the token after %s %s as its value, and the table does not", command, flag)
			}
		}
	}
	for command, flags := range boolean {
		for flag := range flags {
			if takesValue(command, flag) {
				t.Errorf("bd reads %s %s as a boolean, and the table reads the token after it as its value", command, flag)
			}
		}
	}

	// The other way, per command: a row the table holds that bd documents
	// nowhere there is a typo, or a flag bd moved or dropped.
	for command, flags := range valueFlags {
		if value[command] == nil {
			t.Errorf("the table reads a value flag for %q, and bd documents none", command)
		}
		for flag := range flags {
			if !value[command][flag] {
				t.Errorf("the table reads a value for %s %s, and bd documents none", command, flag)
			}
		}
	}
	for command, flags := range boolFlags {
		if boolean[command] == nil {
			t.Errorf("the table calls a flag on %q a boolean, and bd documents no conflict there", command)
		}
		for flag := range flags {
			if !boolean[command][flag] {
				t.Errorf("the table calls %s %s a boolean, and bd documents no conflict there", command, flag)
			}
		}
	}
	for flag := range persistentValueFlags {
		if !persistent[flag] {
			t.Errorf("the table calls %s a persistent value flag, and bd documents none", flag)
		}
	}
	for flag := range persistent {
		if !persistentValueFlags[flag] {
			t.Errorf("bd reads a value for %s on every command, and the table does not", flag)
		}
	}
	for name, command := range aliased {
		if got := aliases[name]; got != command {
			t.Errorf("the table reads the alias %s as %q, and bd reads it as %q", name, got, command)
		}
	}
	for name, command := range aliases {
		if aliased[name] != command {
			t.Errorf("the table reads %s as %s, and bd documents no such alias", name, command)
		}
	}
}

// beadNamingFlags are the long value flags bd documents as taking a bead id.
// Each one must be a row of beadValueFlags, or listed in unroutedBeadFlags.
var beadNamingFlags = map[string]bool{
	"--blocked-by": true, "--blocks": true, "--depends-on": true, "--deps": true,
	"--id": true, "--issue": true, "--of": true, "--parent": true, "--waits-for": true,
	"--with": true,
}

// unroutedBeadFlags are the bead-naming flags bd documents on a command that
// the shim reads as data rather than a bead: the command writes its bead in the
// store its working directory names (see beadValueFlags), or a flag picks the
// store for it (migrate issues, the admin verbs).
var unroutedBeadFlags = map[string]map[string]bool{
	"admin compact":  {"--id": true},
	"create":         {"--deps": true, "--id": true, "--parent": true, "--waits-for": true},
	"create-form":    {"--parent": true},
	"gate create":    {"--blocks": true},
	"migrate issues": {"--id": true},
	"q":              {"--parent": true},
}

// TestBeadValueFlagsCoverWhatBdDocuments checks that every flag the shim reads
// as naming a bead is a value flag bd documents for that command: a typo or a
// flag bd dropped would leave the row reading a token the command never takes a
// value for, and the bead it names would route by the working directory. It
// also checks the other way: a bead-naming flag bd documents on a command must
// be a row, or listed as unrouted with its reason, so a new flag cannot be left
// out of both.
func TestBeadValueFlagsCoverWhatBdDocuments(t *testing.T) {
	t.Parallel()
	value, _, _, _ := bdDocumented(t)
	if len(beadValueFlags) < 10 {
		t.Fatalf("beadValueFlags has %d commands, want at least 10", len(beadValueFlags))
	}
	for command, flags := range beadValueFlags {
		for flag := range flags {
			if !value[command][flag] {
				t.Errorf("the table reads %s %s as naming a bead, and bd documents no such value flag", command, flag)
			}
			if !takesValue(command, flag) {
				t.Errorf("the table reads %s %s as naming a bead, and treats the flag as a boolean", command, flag)
			}
		}
	}
	for command, flags := range value {
		for flag := range flags {
			if beadNamingFlags[flag] && !beadValueFlags[command][flag] && !unroutedBeadFlags[command][flag] {
				t.Errorf("bd documents %s %s as naming a bead, and the table neither reads it as one nor lists it as unrouted", command, flag)
			}
		}
	}
	for command, flags := range unroutedBeadFlags {
		for flag := range flags {
			if !value[command][flag] {
				t.Errorf("unroutedBeadFlags lists %s %s, and bd documents no such value flag", command, flag)
			}
			if beadValueFlags[command][flag] {
				t.Errorf("unroutedBeadFlags and beadValueFlags both list %s %s", command, flag)
			}
		}
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
		{"a bead in a flag's value", []string{"show", "--id", "bt-abc"}, []string{"bt-abc"}},
		{"a bead in an inline flag value", []string{"show", "--id=bt-abc"}, []string{"bt-abc"}},
		{"each id of a comma-separated flag value", []string{"list", "--id", "bt-abc,bt-def"}, []string{"bt-abc", "bt-def"}},
		{"a run id in a flag's value names no bead", []string{"gate", "create", "--await-id", "bt-abc"}, nil},
		{"a parent value names no bead on create", []string{"create", "T", "--parent", "bt-abc"}, nil},
		{"a parent value names no bead on q", []string{"q", "T", "--parent", "bt-abc"}, nil},
		{"a bead flag value after a data operand", []string{"list", "bt-def", "--parent", "bt-abc"}, []string{"bt-def", "bt-abc"}},
		{"an id after a boolean flag", []string{"show", "--json", "bt-abc"}, []string{"bt-abc"}},
		{"an id after a value flag", []string{"update", "--title", "x", "bt-abc"}, []string{"bt-abc"}},
		{"an id past a separator", []string{"show", "--", "bt-abc"}, []string{"bt-abc"}},
		{"a flag-like token past a separator names no bead", []string{"show", "--", "--json"}, nil},
		{"two ids", []string{"dep", "add", "bt-abc", "bt-def"}, []string{"bt-abc", "bt-def"}},
		{"a verb", []string{"list"}, nil},
		{"a short label value", []string{"create", "T", "-l", "review-deep"}, nil},
		{"a short assignee value", []string{"list", "-a", "bt-abc"}, nil},
		{"-f is a value flag on create", []string{"create", "T", "-f", "notes.md", "bt-abc"}, []string{"bt-abc"}},
		{"a title names no bead", []string{"create", "refactor-sling"}, nil},
		{"a title after a value flag names no bead", []string{"create", "--title", "x", "bt-abc"}, nil},
		{"a title that opens with a prefix names no bead", []string{"create", "bt-fix-thing"}, nil},
		{"an id before a data operand", []string{"set-state", "bt-abc", "bt-x=1"}, []string{"bt-abc"}},
		{"each operand of a variadic id is an id", []string{"close", "bt-abc", "zz-def"}, []string{"bt-abc", "zz-def"}},
		{"a label after a variadic id is data", []string{"label", "add", "bt-abc", "refactor-sling"}, []string{"bt-abc"}},
		{"an id past the usage slots still names its bead", []string{"mol", "bond", "bt-a", "bt-b", "zz-c"}, []string{"bt-a", "bt-b", "zz-c"}},
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

// A bead named by a flag value reaches the tracker from a Dolt rig's directory,
// and a create that names a tracker parent still runs in the store its
// directory names: the two halves of gt-b00kz's acceptance criterion, through
// the entry point rather than through Decide alone.
func TestMainRoutesABeadValueFlag(t *testing.T) {
	t.Parallel()
	town := newTown(t, routeTown, routeAlpha, routeBeta)
	alpha := filepath.Join(town, "alpha", "mayor", "rig")

	// show --id bt-abc from the dolt rig reaches the tracker's handler.
	answered, ran := 0, &fakeProcess{}
	routed := map[string]Handler{"show": func(Env, Decision) int { answered++; return 0 }}
	e, _, _ := shimEnv(t, alpha, "", ran, routed, "show", "--id", "bt-abc")
	if got := Main(e); got != 0 {
		t.Errorf("Main(show --id bt-abc) = %d, want the tracker handler's 0", got)
	}
	if answered != 1 || ran.ran != nil {
		t.Errorf("Main(show --id bt-abc) answered %d times and ran %v, want one handler call and no bd.real", answered, ran.ran)
	}

	// create T --parent bt-abc from the dolt rig reaches bd.real unchanged.
	args := []string{"create", "T", "--parent", "bt-abc"}
	proc := &fakeProcess{}
	e, stderr, shimPath := shimEnv(t, alpha, "", proc, routed, args...)
	if got := Main(e); got != 0 {
		t.Errorf("Main(%v) = %d, want bd.real's 0 (stderr %q)", args, got, stderr.String())
	}
	if strings.Join(proc.ran, " ") != strings.Join(args, " ") {
		t.Errorf("Main(%v) ran %v, want bd's argv unchanged", args, proc.ran)
	}
	if want := filepath.Join(filepath.Dir(shimPath), realName); proc.path != want {
		t.Errorf("Main(%v) ran %q, want bd.real at %q", args, proc.path, want)
	}
	if answered != 1 {
		t.Errorf("Main(%v) reached a tracker handler, want bd.real", args)
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
