package land

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/nodedeps"
)

// staleNodeTree is a tree whose installed eslint-plugin-react-hooks sits a
// version behind its package-lock.json: fr-358's shape, where make lint was
// clean on the worktree and the landing gate's npm ci was not (gt-wd12s).
func staleNodeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "package-lock.json", `{
	  "name": "app", "lockfileVersion": 3,
	  "packages": {
	    "": {"name": "app", "version": "1.0.0"},
	    "node_modules/eslint-plugin-react-hooks": {"version": "7.1.1"}
	  }
	}`)
	writeFile(t, dir, "node_modules/eslint-plugin-react-hooks/package.json",
		`{"name":"eslint-plugin-react-hooks","version":"7.0.1"}`)
	return dir
}

// okRun is the step runner of a gate whose one step prints line and passes.
func okRun(line string, out *bytes.Buffer) runFunc {
	return func(_ context.Context, _ string, _, _ []string, w io.Writer) (int, error) {
		if out != nil {
			_, _ = io.WriteString(w, line)
		}
		return 0, nil
	}
}

// TestCommandGateReportsStaleNodeModulesBeforeItsSteps: the pre-submit tier
// reports the tree's node_modules disagreeing with its lockfile — naming the
// package — before any step runs, and the gate still passes: an out-of-date
// package is not a verdict on the change.
func TestCommandGateReportsStaleNodeModulesBeforeItsSteps(t *testing.T) {
	t.Parallel()
	dir := staleNodeTree(t)
	var out bytes.Buffer
	g := CommandGate{
		Steps:         []Step{{Name: "lint", Command: "make lint"}},
		Out:           &out,
		CheckNodeDeps: true,
	}
	g.run = okRun("lint ok\n", &out)
	res := g.Run(context.Background(), dir)
	if !res.Passed || res.Err != nil {
		t.Fatalf("gate = %+v, want passed", res)
	}
	printed := out.String()
	if !strings.Contains(printed, "gate: WARNING node_modules disagrees with package-lock.json") {
		t.Errorf("gate output lacks the stale-deps warning:\n%s", printed)
	}
	if !strings.Contains(printed, "node_modules/eslint-plugin-react-hooks installed 7.0.1, lockfile pins 7.1.1") {
		t.Errorf("the warning does not name the mismatched package and both versions:\n%s", printed)
	}
	if !strings.Contains(printed, "npm ci") {
		t.Errorf("the warning does not say how to reproduce the gate's tree:\n%s", printed)
	}
	// The warning is the gate's word on the tree before the steps it
	// qualifies, not something a reader finds after their output.
	warnAt, stepAt := strings.Index(printed, "gate: WARNING"), strings.Index(printed, "lint ok")
	if warnAt < 0 || stepAt < 0 || warnAt > stepAt {
		t.Errorf("warning at %d, step output at %d: want the warning first:\n%s", warnAt, stepAt, printed)
	}
	warns := res.Warnings()
	if len(warns) == 0 || !strings.Contains(warns[0], "eslint-plugin-react-hooks") {
		t.Errorf("result warnings = %q, want the preflight kept for the record", warns)
	}
}

// TestCommandGateStaysQuietOnANodeTreeThatAgrees: a worktree installed from
// the lockfile it carries says nothing.
func TestCommandGateStaysQuietOnANodeTreeThatAgrees(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "package-lock.json", `{"lockfileVersion": 3, "packages": {"node_modules/left-pad": {"version": "1.0.0"}}}`)
	writeFile(t, dir, "node_modules/left-pad/package.json", `{"name":"left-pad","version":"1.0.0"}`)
	var out bytes.Buffer
	g := CommandGate{
		Steps:         []Step{{Name: "lint", Command: "make lint"}},
		Out:           &out,
		CheckNodeDeps: true,
	}
	g.run = okRun("lint ok\n", &out)
	res := g.Run(context.Background(), dir)
	if !res.Passed {
		t.Fatalf("gate = %+v, want passed", res)
	}
	if strings.Contains(out.String(), "gate: WARNING") {
		t.Errorf("a tree in sync with its lockfile warned:\n%s", out.String())
	}
}

// TestCommandGateWithoutCheckNodeDepsIgnoresNodeModules: the landing gate runs
// on the lander host's tree, so it does not report the author's node_modules.
func TestCommandGateWithoutCheckNodeDepsIgnoresNodeModules(t *testing.T) {
	t.Parallel()
	dir := staleNodeTree(t)
	var out bytes.Buffer
	g := CommandGate{
		Steps: []Step{{Name: "lint", Command: "make lint"}},
		Out:   &out,
	}
	g.run = okRun("lint ok\n", &out)
	res := g.Run(context.Background(), dir)
	if !res.Passed {
		t.Fatalf("gate = %+v, want passed", res)
	}
	if strings.Contains(out.String(), "node_modules disagrees") {
		t.Errorf("a gate without CheckNodeDeps reported node_modules:\n%s", out.String())
	}
	if warns := res.Warnings(); len(warns) != 0 {
		t.Errorf("result warnings = %q, want none", warns)
	}
}

// TestNodeDepsDetailNamesThreeAtMostAndCountsTheRest: a tree with hundreds of
// stale packages gets a warning a reader can act on, not a wall of paths.
func TestNodeDepsDetailNamesThreeAtMostAndCountsTheRest(t *testing.T) {
	t.Parallel()
	var mismatches []nodedeps.Mismatch
	for _, name := range []string{"alpha", "beta", "gamma", "delta"} {
		mismatches = append(mismatches, nodedeps.Mismatch{
			Path:      "node_modules/" + name,
			Installed: "1.0.0",
			Locked:    "2.0.0",
		})
	}
	got := nodeDepsDetail(mismatches)
	for _, want := range []string{"node_modules/alpha installed 1.0.0, lockfile pins 2.0.0", "node_modules/gamma", "1 more"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "delta") {
		t.Errorf("detail %q names a package past the third instead of counting it", got)
	}
}

// TestCommandGateReportsARequiredPinTheTreeNeverInstalled: a lockfile change
// that added a dependency leaves a worktree whose install predates it with
// nothing there at all, which is the same stale verdict as a wrong version.
func TestCommandGateReportsARequiredPinTheTreeNeverInstalled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "package-lock.json", `{
	  "lockfileVersion": 3,
	  "packages": {
	    "node_modules/eslint-plugin-x": {"version": "3.1.0"},
	    "node_modules/installed": {"version": "1.0.0"}
	  }
	}`)
	writeFile(t, dir, "node_modules/installed/package.json", `{"name":"installed","version":"1.0.0"}`)
	var out bytes.Buffer
	g := CommandGate{
		Steps:         []Step{{Name: "lint", Command: "make lint"}},
		Out:           &out,
		CheckNodeDeps: true,
	}
	g.run = okRun("lint ok\n", &out)
	if res := g.Run(context.Background(), dir); !res.Passed {
		t.Fatalf("gate = %+v, want passed", res)
	}
	if !strings.Contains(out.String(), "node_modules/eslint-plugin-x not installed, lockfile pins 3.1.0") {
		t.Errorf("the warning does not name the pin the tree never installed:\n%s", out.String())
	}
}

// TestCommandGateReportsAPackageItCouldNotCompare: an entry the comparison
// reached no verdict on is reported as such, beside the mismatches it found.
func TestCommandGateReportsAPackageItCouldNotCompare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "package-lock.json", `{
	  "lockfileVersion": 3,
	  "packages": {
	    "node_modules/broken": {"version": "1.0.0"},
	    "node_modules/stale": {"version": "2.0.0"}
	  }
	}`)
	writeFile(t, dir, "node_modules/broken/package.json", `{"version": `)
	writeFile(t, dir, "node_modules/stale/package.json", `{"name":"stale","version":"1.0.0"}`)
	var out bytes.Buffer
	g := CommandGate{
		Steps:         []Step{{Name: "lint", Command: "make lint"}},
		Out:           &out,
		CheckNodeDeps: true,
	}
	g.run = okRun("lint ok\n", &out)
	if res := g.Run(context.Background(), dir); !res.Passed {
		t.Fatalf("gate = %+v, want passed", res)
	}
	printed := out.String()
	if !strings.Contains(printed, "node_modules/stale installed 1.0.0") {
		t.Errorf("an unreadable entry hid the mismatches the check reached:\n%s", printed)
	}
	if !strings.Contains(printed, "1 package(s) in node_modules could not be compared with package-lock.json: node_modules/broken") {
		t.Errorf("the entry with no verdict went unreported:\n%s", printed)
	}
}

// TestCommandGateReportsALockfileItCouldNotCompare: a lockfile the check
// cannot read is reported as a comparison that did not happen, never as a
// tree in sync.
func TestCommandGateReportsALockfileItCouldNotCompare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "package-lock.json", `{"packages": `)
	writeFile(t, dir, "node_modules/left-pad/package.json", `{"name":"left-pad","version":"1.0.0"}`)
	var out bytes.Buffer
	g := CommandGate{
		Steps:         []Step{{Name: "lint", Command: "make lint"}},
		Out:           &out,
		CheckNodeDeps: true,
	}
	g.run = okRun("lint ok\n", &out)
	res := g.Run(context.Background(), dir)
	if !res.Passed {
		t.Fatalf("gate = %+v, want passed: the check is a warning, not a step", res)
	}
	if !strings.Contains(out.String(), "gate: WARNING could not compare node_modules with package-lock.json") {
		t.Errorf("an unreadable lockfile went unreported:\n%s", out.String())
	}
}
