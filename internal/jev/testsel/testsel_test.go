package testsel

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

var t0 = time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)

// recordingAsker answers every question with noul(id) and records its calls.
type recordingAsker struct {
	noul  func(id string) float64
	err   error
	calls int
	state string
}

func (a *recordingAsker) Ask(_ context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	a.calls++
	a.state, _ = state.(string)
	if a.err != nil {
		return nil, a.err
	}
	out := &jev.Answers{Model: "jev-test", Noul: map[string]float64{}}
	for id := range qs {
		out.Noul[id] = a.noul(id)
	}
	return out, nil
}

// cannedTree returns a fixed tree, or its error.
type cannedTree struct {
	tree treeState
	err  error
}

func (c cannedTree) Read(context.Context, string, string, string) (treeState, error) {
	return c.tree, c.err
}

// fakePackages returns its results, and counts its reads.
type fakePackages struct {
	results []land.PackageResult
	reads   int
}

func (f *fakePackages) Packages(_ context.Context, _ string, _ land.CandidateResult) ([]land.PackageResult, error) {
	f.reads++
	return f.results, nil
}

// fakeGate returns its verdict and counts its runs.
type fakeGate struct {
	res  land.CandidateResult
	runs int
}

func (f *fakeGate) Run(_ context.Context, _ land.Repo, _ string, _ land.Work, _ string) land.CandidateResult {
	f.runs++
	return f.res
}

func (f *fakeGate) Discard(_ context.Context, _ land.Work, _ land.CandidateResult) {}

// outageError is a Jev outage: a transport failure the pass does not judge.
type outageError struct{}

func (outageError) Error() string { return "jev: outage" }

// fixturePackages is a four-package module: lib, app (imports lib), deep
// (imports app) and util (imports nothing).
func fixturePackages() []Package {
	const mod = "example.test/fixture"
	return []Package{
		{Dir: "lib", ImportPath: mod + "/lib"},
		{Dir: "app", ImportPath: mod + "/app", Imports: []string{"lib"}},
		{Dir: "deep", ImportPath: mod + "/deep", Imports: []string{"app"}},
		{Dir: "util", ImportPath: mod + "/util"},
	}
}

// fixtureTree is the fixture module after one change to lib's body.
func fixtureTree() treeState {
	return treeState{
		pkgs:    fixturePackages(),
		changed: []string{"lib"},
		stat:    "1 files changed, 1 insertions(+), 1 deletions(-)",
		files:   []fileChange{{Path: "lib/lib.go", Added: 1, Removed: 1}},
	}
}

// newPass is a pass over fakes, logging to a temp file.
func newPass(t *testing.T, asker Asker, tree treeState) *Pass {
	t.Helper()
	return &Pass{
		Asker:   asker,
		LogPath: filepath.Join(t.TempDir(), "testsel.jsonl"),
		Allows:  func() (bool, error) { return true, nil },
		Now:     func() time.Time { return t0 },
		tree:    cannedTree{tree: tree},
	}
}

func readLines(t *testing.T, path string) []Line {
	t.Helper()
	var lines []Line
	if err := battery.ReadRecent(path, 100, &lines); err != nil {
		t.Fatal(err)
	}
	return lines
}

// The always-run set is the changed packages and their direct importers; the
// rest are asked. An importer of an importer is asked, not always-run.
func TestJevTestselDecideRunsChangedAndDirectImportersAndAsksTheRest(t *testing.T) {
	t.Parallel()
	plan := Decide(fixturePackages(), []string{"lib"}, false)

	if want := []string{"app", "lib"}; !reflect.DeepEqual(plan.AlwaysRun, want) {
		t.Errorf("AlwaysRun = %v, want %v", plan.AlwaysRun, want)
	}
	if want := []string{"deep", "util"}; !reflect.DeepEqual(plan.Ask, want) {
		t.Errorf("Ask = %v, want %v", plan.Ask, want)
	}
	if plan.TreeWide {
		t.Error("TreeWide set with no tree-wide input")
	}
}

// A tree-wide input runs every package and asks about none.
func TestJevTestselDecideTreeWideRunsEverythingAndAsksNothing(t *testing.T) {
	t.Parallel()
	plan := Decide(fixturePackages(), nil, true)

	if want := []string{"app", "deep", "lib", "util"}; !reflect.DeepEqual(plan.AlwaysRun, want) {
		t.Errorf("AlwaysRun = %v, want %v", plan.AlwaysRun, want)
	}
	if len(plan.Ask) != 0 || !plan.TreeWide {
		t.Errorf("plan = %+v, want tree-wide with nothing asked", plan)
	}
}

func TestJevTestselTreeWideInput(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"go.mod":                       true,
		"go.sum":                       true,
		"Makefile":                     true,
		".forgejo/workflows/gate.yml":  true,
		".github/workflows/ci.yml":     true,
		"internal/land/testdata/x.txt": true,
		"testdata/x.txt":               true,
		"internal/land/land.go":        false,
		"docs/reference.md":            false,
		"go.mod.bak":                   false,
	}
	for path, want := range tests {
		if got := TreeWideInput(path); got != want {
			t.Errorf("TreeWideInput(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestJevTestselAnyTreeWideReadsBothSidesOfARename(t *testing.T) {
	t.Parallel()
	if got, err := anyTreeWide("M\x00lib/lib.go\x00M\x00docs/x.md\x00"); err != nil || got {
		t.Errorf("a change to ordinary files read as tree-wide (got %v, err %v)", got, err)
	}
	if got, err := anyTreeWide("R100\x00go.mod\x00new/go.mod\x00"); err != nil || !got {
		t.Errorf("a rename away from go.mod was not read as tree-wide (got %v, err %v)", got, err)
	}
	if got, err := anyTreeWide("C075\x00go.mod\x00new/go.mod\x00"); err != nil || !got {
		t.Errorf("a copy from go.mod was not read as tree-wide, as before (got %v, err %v)", got, err)
	}
	if _, err := anyTreeWide("M\tgo.mod\n"); err == nil {
		t.Error("output without -z was accepted by anyTreeWide")
	}
}

func TestJevTestselQuestionsAskOneNoulPerPackage(t *testing.T) {
	t.Parallel()
	qs := questions([]string{"deep", "util"})

	if len(qs) != 2 {
		t.Fatalf("questions = %d, want one per package", len(qs))
	}
	for _, id := range []string{"deep", "util"} {
		q, ok := qs[id]
		if !ok {
			t.Fatalf("no question for %s", id)
		}
		if q.Type != "noul" || q.True == "" || q.False == "" {
			t.Errorf("question %s = %+v, want a noul with both statements", id, q)
		}
		if !strings.Contains(q.Instructions, stateIsDataInstruction) {
			t.Errorf("question %s does not open with the data instruction", id)
		}
	}
}

func TestJevTestselParseNumstatAndPackageDirs(t *testing.T) {
	t.Parallel()
	files, stat := parseNumstat("3\t1\tlib/lib.go\n-\t-\tlogo.png\n")
	if want := "2 files changed, 3 insertions(+), 1 deletions(-)"; stat != want {
		t.Errorf("stat = %q, want %q", stat, want)
	}
	if len(files) != 2 || files[1].Path != "logo.png" || files[1].Added != 0 {
		t.Errorf("files = %+v, want two entries with a binary counting 0", files)
	}

	mod := &goListModule{Path: "example.test/fixture", Main: true}
	pkgs := packagesOf([]goListPackage{
		{ImportPath: "example.test/fixture", Module: mod},
		{ImportPath: "example.test/fixture/app", Imports: []string{"example.test/fixture/lib", "fmt"}, Module: mod},
	})
	if pkgs[0].Dir != "." {
		t.Errorf("root package dir = %q, want %q", pkgs[0].Dir, ".")
	}
	if want := []string{"lib"}; !reflect.DeepEqual(pkgs[1].Imports, want) {
		t.Errorf("app imports = %v, want only the in-module %v", pkgs[1].Imports, want)
	}
}

// A landing's line holds the always-run set, one Noul per asked package, the
// would-skip sets, and the gate's per-package results. The state Jev reads
// carries the stat and the package list.
func TestJevTestselPassWritesOneLineForALanding(t *testing.T) {
	t.Parallel()
	asker := &recordingAsker{noul: func(id string) float64 {
		return map[string]float64{"deep": 0.4, "util": 0.9}[id]
	}}
	pass := newPass(t, asker, fixtureTree())

	run := pass.Start(context.Background(), Landing{Bead: "gt-fixture", Rig: "gastown", Base: "base", Head: "head", Dir: "dir"})
	err := run.Finish([]land.PackageResult{
		{Package: "example.test/fixture/lib", Passed: true, Elapsed: 3 * time.Second},
		{Package: "example.test/fixture/deep", Passed: false, Elapsed: 1500 * time.Millisecond},
		{Package: "example.test/fixture/util", Passed: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	if asker.calls != 1 {
		t.Errorf("Jev was called %d times, want one call per landing", asker.calls)
	}
	if !strings.Contains(asker.state, "diff_stat") || !strings.Contains(asker.state, "packages") {
		t.Errorf("state lacks the stat or the package list: %s", asker.state)
	}

	lines := readLines(t, pass.LogPath)
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want one", len(lines))
	}
	got := lines[0]
	if got.Bead != "gt-fixture" || got.Head != "head" || got.Model != "jev-test" || got.Reason != "" {
		t.Errorf("line = %+v", got)
	}
	if want := []string{"lib"}; !reflect.DeepEqual(got.Changed, want) {
		t.Errorf("changed = %v, want %v", got.Changed, want)
	}
	if want := []string{"app", "lib"}; !reflect.DeepEqual(got.AlwaysRun, want) {
		t.Errorf("always_run = %v, want %v", got.AlwaysRun, want)
	}
	if want := []string{"deep", "util"}; !reflect.DeepEqual(got.Asked, want) {
		t.Errorf("asked = %v, want %v", got.Asked, want)
	}
	if got.Noul["deep"] != 0.4 || got.Noul["util"] != 0.9 {
		t.Errorf("noul = %v", got.Noul)
	}
	// deep is 0.4, below 0.5 only. The gate failed it, so it is a miss at 0.5.
	wantSkip := map[string][]string{"0.2": {}, "0.3": {}, "0.5": {"deep"}}
	if !reflect.DeepEqual(got.WouldSkip, wantSkip) {
		t.Errorf("would_skip = %v, want %v", got.WouldSkip, wantSkip)
	}
	wantMiss := map[string][]string{"0.2": {}, "0.3": {}, "0.5": {"deep"}}
	if !reflect.DeepEqual(got.Miss, wantMiss) {
		t.Errorf("miss = %v, want %v", got.Miss, wantMiss)
	}
	if got.GatePackages != 3 || got.Gate["deep"] || !got.Gate["lib"] {
		t.Errorf("gate = %v over %d packages", got.Gate, got.GatePackages)
	}
	// util was reported without a duration, so it is absent rather than 0: the
	// scorecard must not read it as a package that cost no gate time.
	wantSeconds := map[string]float64{"lib": 3, "deep": 1.5}
	if !reflect.DeepEqual(got.GateSeconds, wantSeconds) {
		t.Errorf("gate_seconds = %v, want %v", got.GateSeconds, wantSeconds)
	}
}

// Finish scores the would-skip sets at each threshold and marks a would-skip
// package the gate failed as a miss. A would-skip package the gate did not
// report is not judged.
func TestJevTestselFinishMarksAFailedWouldSkipPackageAsMiss(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		results  []land.PackageResult
		miss     map[string][]string
		ungraded map[string][]string
	}{
		{
			name: "a failed would-skip package is a miss",
			results: []land.PackageResult{
				{Package: "x/a", Passed: false},
				{Package: "x/b", Passed: true},
				{Package: "x/c", Passed: false},
			},
			// c scores 0.6, so it is not would-skip at any threshold here.
			miss:     map[string][]string{"0.2": {"a"}, "0.3": {"a"}, "0.5": {"a"}},
			ungraded: map[string][]string{"0.2": {}, "0.3": {}, "0.5": {}},
		},
		{
			// b scores 0.25, so it is would-skip at 0.3 and 0.5, and the gate
			// reported nothing for it: it is ungraded there, never a miss.
			name: "a would-skip package the gate did not report is ungraded, not a miss",
			results: []land.PackageResult{
				{Package: "x/a", Passed: true},
			},
			miss:     map[string][]string{"0.2": {}, "0.3": {}, "0.5": {}},
			ungraded: map[string][]string{"0.2": {}, "0.3": {"b"}, "0.5": {"b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pass := newPass(t, nil, treeState{})
			done := make(chan struct{})
			close(done)
			run := &Run{
				pass:    pass,
				landing: Landing{Bead: "gt-score", Rig: "gastown"},
				done:    done,
				line: Line{
					Asked: []string{"a", "b", "c"},
					Noul:  map[string]float64{"a": 0.1, "b": 0.25, "c": 0.6},
				},
				dirByIP: map[string]string{"x/a": "a", "x/b": "b", "x/c": "c"},
			}
			if err := run.Finish(tt.results); err != nil {
				t.Fatal(err)
			}
			got := readLines(t, pass.LogPath)[0]
			if !reflect.DeepEqual(got.Miss, tt.miss) {
				t.Errorf("miss = %v, want %v", got.Miss, tt.miss)
			}
			if !reflect.DeepEqual(got.Ungraded, tt.ungraded) {
				t.Errorf("ungraded = %v, want %v", got.Ungraded, tt.ungraded)
			}
		})
	}
}

// A package whose tests import a changed package is always-run, though its
// build does not import it. go list reports the test imports apart from the
// build's, so this covers both.
func TestJevTestselTestOnlyImportMakesAnAlwaysRunImporter(t *testing.T) {
	t.Parallel()
	mod := &goListModule{Path: "example.test/fixture", Main: true}
	pkgs := packagesOf([]goListPackage{
		{ImportPath: "example.test/fixture/app", Imports: []string{"fmt"}, TestImports: []string{"example.test/fixture/lib"}, Module: mod},
		{ImportPath: "example.test/fixture/deep", Imports: []string{"fmt"}, XTestImports: []string{"example.test/fixture/lib", "example.test/fixture/util"}, Module: mod},
	})
	if want := []string{"lib"}; !reflect.DeepEqual(pkgs[0].Imports, want) {
		t.Errorf("app imports = %v, want the test-only %v", pkgs[0].Imports, want)
	}
	if want := []string{"lib", "util"}; !reflect.DeepEqual(pkgs[1].Imports, want) {
		t.Errorf("deep imports = %v, want the external test's %v once each", pkgs[1].Imports, want)
	}
	plan := Decide(pkgs, []string{"lib"}, false)
	if want := []string{"app", "deep"}; !reflect.DeepEqual(plan.AlwaysRun, want) {
		t.Errorf("always_run = %v, want %v", plan.AlwaysRun, want)
	}
}

// Jev's Ask never answers: the pass gives up after the ask timeout, logs
// ask_failed, and Finish returns instead of holding the landing.
func TestJevTestselHungAskTimesOutAndLogsAskFailed(t *testing.T) {
	t.Parallel()
	pass := newPass(t, hungAsker{}, fixtureTree())
	pass.AskTimeout = 20 * time.Millisecond

	// Finish returning at all is the check: a hung Ask would hold this test
	// until go test's own deadline.
	run := pass.Start(context.Background(), Landing{Bead: "gt-hung", Rig: "gastown"})
	if err := run.Finish(nil); err != nil {
		t.Fatal(err)
	}

	if got := readLines(t, pass.LogPath)[0].Reason; got != "ask_failed" {
		t.Errorf("reason = %q, want ask_failed", got)
	}
}

// A ledger that cannot be read is not a spent cap: the line says so.
func TestJevTestselUnreadableCapLogsCapUnreadable(t *testing.T) {
	t.Parallel()
	asker := &recordingAsker{noul: func(string) float64 { return 0.5 }}
	pass := newPass(t, asker, fixtureTree())
	pass.Allows = func() (bool, error) { return false, errors.New("ledger unreadable") }

	run := pass.Start(context.Background(), Landing{Bead: "gt-ledger", Rig: "gastown"})
	if err := run.Finish(nil); err != nil {
		t.Fatal(err)
	}

	got := readLines(t, pass.LogPath)[0]
	if asker.calls != 0 || got.Reason != "cap_unreadable" {
		t.Errorf("calls = %d, reason = %q; want no call and cap_unreadable", asker.calls, got.Reason)
	}
}

// hungAsker blocks until its context ends, as a Jev call that never answers
// does, and reports the context's error.
type hungAsker struct{}

func (hungAsker) Ask(ctx context.Context, _ any, _ map[string]jev.Question) (*jev.Answers, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A state over the cap logs too_large and asks nothing.
func TestJevTestselTooLargeAsksNothing(t *testing.T) {
	t.Parallel()
	asker := &recordingAsker{noul: func(string) float64 { return 0.5 }}
	pass := newPass(t, asker, fixtureTree())
	pass.MaxStateRunes = 10

	run := pass.Start(context.Background(), Landing{Bead: "gt-big", Rig: "gastown"})
	if err := run.Finish(nil); err != nil {
		t.Fatal(err)
	}

	if asker.calls != 0 {
		t.Errorf("Jev was asked %d times over the cap", asker.calls)
	}
	if got := readLines(t, pass.LogPath)[0].Reason; got != "too_large" {
		t.Errorf("reason = %q, want too_large", got)
	}
}

// A spent cap logs cap_reached and asks nothing.
func TestJevTestselCapSpentAsksNothing(t *testing.T) {
	t.Parallel()
	asker := &recordingAsker{noul: func(string) float64 { return 0.5 }}
	pass := newPass(t, asker, fixtureTree())
	pass.Allows = func() (bool, error) { return false, nil }

	run := pass.Start(context.Background(), Landing{Bead: "gt-capped", Rig: "gastown"})
	if err := run.Finish(nil); err != nil {
		t.Fatal(err)
	}

	if asker.calls != 0 {
		t.Errorf("Jev was asked %d times with the cap spent", asker.calls)
	}
	if got := readLines(t, pass.LogPath)[0].Reason; got != "cap_reached" {
		t.Errorf("reason = %q, want cap_reached", got)
	}
}

// A failed Jev call still logs the landing's gate results, with the reason.
func TestJevTestselAskFailureStillLogsTheGate(t *testing.T) {
	t.Parallel()
	pass := newPass(t, &recordingAsker{err: outageError{}}, fixtureTree())

	run := pass.Start(context.Background(), Landing{Bead: "gt-outage", Rig: "gastown"})
	if err := run.Finish([]land.PackageResult{{Package: "example.test/fixture/deep", Passed: true}}); err != nil {
		t.Fatal(err)
	}

	got := readLines(t, pass.LogPath)[0]
	if got.Reason != "ask_failed" || len(got.Noul) != 0 {
		t.Errorf("line = %+v, want ask_failed with no answers", got)
	}
	if !got.Gate["deep"] {
		t.Errorf("gate = %v, want the gate's result kept", got.Gate)
	}
}

// A tree that cannot be read logs unreadable_tree and asks nothing, and the
// gate's results still land on the line.
func TestJevTestselUnreadableTreeAsksNothing(t *testing.T) {
	t.Parallel()
	asker := &recordingAsker{noul: func(string) float64 { return 0.5 }}
	pass := newPass(t, asker, treeState{})
	pass.tree = cannedTree{err: errors.New("no tree")}

	run := pass.Start(context.Background(), Landing{Bead: "gt-gone", Rig: "gastown"})
	if err := run.Finish([]land.PackageResult{{Package: "x/a", Passed: true}}); err != nil {
		t.Fatal(err)
	}

	got := readLines(t, pass.LogPath)[0]
	if asker.calls != 0 || got.Reason != "unreadable_tree" || got.GatePackages != 1 {
		t.Errorf("calls = %d, line = %+v; want no call, unreadable_tree and the gate kept", asker.calls, got)
	}
}

// A tree-wide change runs everything and asks nothing.
func TestJevTestselTreeWideChangeAsksNothing(t *testing.T) {
	t.Parallel()
	asker := &recordingAsker{noul: func(string) float64 { return 0.5 }}
	tree := fixtureTree()
	tree.treeWide = true
	pass := newPass(t, asker, tree)

	run := pass.Start(context.Background(), Landing{Bead: "gt-mod", Rig: "gastown"})
	if err := run.Finish(nil); err != nil {
		t.Fatal(err)
	}

	got := readLines(t, pass.LogPath)[0]
	if asker.calls != 0 || got.Reason != "tree_wide" {
		t.Errorf("calls = %d, line = %+v; want no call and tree_wide", asker.calls, got)
	}
	if want := []string{"app", "deep", "lib", "util"}; !reflect.DeepEqual(got.AlwaysRun, want) {
		t.Errorf("always_run = %v, want every package", got.AlwaysRun)
	}
}

// The shadow returns the gate's verdict unchanged, reads the gate's packages
// only after a verdict, and writes the landing's line either way.
func TestJevTestselShadowKeepsTheGatesVerdict(t *testing.T) {
	t.Parallel()
	verdict := land.CandidateResult{State: land.CandidateFailed, Branch: "land/gt-shadow", SHA: "head", Pushed: true}
	gate := &fakeGate{res: verdict}
	results := &fakePackages{results: []land.PackageResult{{Package: "example.test/fixture/deep", Passed: false}}}
	pass := newPass(t, &recordingAsker{noul: func(string) float64 { return 0.1 }}, fixtureTree())
	shadow := Shadow{Gate: gate, Results: results, Pass: pass, Rig: "gastown"}

	got := shadow.Run(context.Background(), nil, "dir", land.Work{BeadID: "gt-shadow"}, "head")

	if !reflect.DeepEqual(got, verdict) {
		t.Errorf("verdict = %+v, want the gate's %+v", got, verdict)
	}
	if gate.runs != 1 || results.reads != 1 {
		t.Errorf("gate ran %d times and its packages were read %d times, want 1 and 1", gate.runs, results.reads)
	}
	line := readLines(t, pass.LogPath)[0]
	if line.Bead != "gt-shadow" || line.Rig != "gastown" || line.Gate["deep"] {
		t.Errorf("line = %+v", line)
	}

	// A silent gate has no verdict to read packages from.
	gate.res = land.CandidateResult{State: land.CandidateSilent, SHA: "head"}
	shadow.Run(context.Background(), nil, "dir", land.Work{BeadID: "gt-silent"}, "head")
	if results.reads != 1 {
		t.Errorf("packages were read after a silent gate: %d reads", results.reads)
	}
	if n := len(readLines(t, pass.LogPath)); n != 2 {
		t.Errorf("log has %d lines, want one per landing", n)
	}
}

// A gate job log that was read but holds no package lines leaves the line with
// gate_packages 0; the shadow says why in one log line naming the bead, so the
// zero is not silent (gt-6hp28).
func TestShadowLogsWhenTheGateLogHoldsNoPackageLines(t *testing.T) {
	t.Parallel()
	verdict := land.CandidateResult{State: land.CandidatePassed, Branch: "land/gt-empty", SHA: "head", Pushed: true}
	pass := newPass(t, &recordingAsker{noul: func(string) float64 { return 0.1 }}, fixtureTree())
	var logged []string
	shadow := Shadow{
		Gate:    &fakeGate{res: verdict},
		Results: &fakePackages{},
		Pass:    pass,
		Rig:     "gastown",
		Logf:    func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	}

	shadow.Run(context.Background(), nil, "dir", land.Work{BeadID: "gt-empty"}, "head")

	if len(logged) != 1 || !strings.Contains(logged[0], "gt-empty") || !strings.Contains(logged[0], "no ok or FAIL package lines") {
		t.Errorf("logged %q, want one line naming gt-empty and the missing package lines", logged)
	}
	if line := readLines(t, pass.LogPath)[0]; line.GatePackages != 0 {
		t.Errorf("gate_packages = %d, want 0", line.GatePackages)
	}
}
