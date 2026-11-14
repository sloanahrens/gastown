package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

func TestSlingNoteShowsGitCallsOnTheStepThatMadeThem(t *testing.T) {
	t.Parallel()
	clock := time.Unix(1_000_000, 0)
	now := func() time.Time { return clock }
	trace := session.NewSandboxTrace(now)
	// The spawn sites hand trace.CountGit to git.WithRunObserver, so the
	// callback here is the one a git process calls.
	observe := trace.CountGit

	if err := trace.Phase("seat decision", func() error {
		clock = clock.Add(time.Second)
		observe()
		observe()
		return nil
	}); err != nil {
		t.Fatalf("seat decision phase: %v", err)
	}
	if err := trace.Phase("worktree", func() error {
		clock = clock.Add(2 * time.Second)
		observe()
		return nil
	}); err != nil {
		t.Fatalf("worktree phase: %v", err)
	}

	want := "sling gt-a seat box: seat decision 1s [bd 0, git 2], worktree 2s [bd 0, git 1], total 3s; outcome: started"
	if got := specSlingTraceLine("gt-a", "box", trace, nil); got != want {
		t.Fatalf("specSlingTraceLine() = %q, want %q: each step shows the git calls it made", got, want)
	}
}

func TestSlingCallsOfTwoSlingsCountSeparately(t *testing.T) {
	t.Parallel()
	clock := time.Unix(1_000_000, 0)
	now := func() time.Time { return clock }
	first := session.NewSandboxTrace(now)
	second := session.NewSandboxTrace(now)
	observeFirst, observeSecond := first.CountGit, second.CountGit

	// The two slings' calls interleave, as two dispatches running together do.
	observeFirst()
	observeSecond()
	observeFirst()
	observeFirst()
	for _, tr := range []*session.SandboxTrace{first, second} {
		if err := tr.Phase("seat", func() error { return nil }); err != nil {
			t.Fatalf("seat phase: %v", err)
		}
	}

	if got, want := first.Summary(), "seat 0s [bd 0, git 3], total 0s"; got != want {
		t.Fatalf("first sling Summary() = %q, want %q", got, want)
	}
	if got, want := second.Summary(), "seat 0s [bd 0, git 1], total 0s"; got != want {
		t.Fatalf("second sling Summary() = %q, want %q: the other sling's calls must not land here", got, want)
	}
}

func TestSlingCallsNilTraceCountsNothingAndDoesNotPanic(t *testing.T) {
	t.Parallel()
	var trace *session.SandboxTrace
	// A spawn with no trace (a hand-run gt sling) passes this callback.
	observe := trace.CountGit
	observe()
	observe()

	if got := trace.Summary(); got != "" {
		t.Fatalf("nil trace Summary() = %q, want empty: a sling with no trace logs nothing", got)
	}
}

// spawnBuildCounts is what the scanner found in one function: how many git
// handles or managers it builds, split by whether the construction observes
// them.
type spawnBuildCounts struct {
	observed   int
	unobserved int
}

// spawnGitSite is what one function in polecat_spawn.go does with the git
// handles it builds: how many, and whether each construction hands the handle
// to the sling's run observer on the same statement.
type spawnGitSite struct {
	count    int    // git.NewGit / git.NewGitWithDir calls in the function
	observed bool   // each one's statement carries .WithRunObserver
	reason   string // why an unobserved construction carries no trace
}

// spawnPathGitSites names every git handle polecat_spawn.go builds. A new
// construction that forgets the observer fails the guard instead of going
// uncounted, and so does a known site whose observer is removed (gt-74tzuk).
// The two functions listed with a reason carry no trace: IsRigName resolves a
// `gt sling` command-line target (slingRun's isRigName and
// workflowTargetOverride), and that command path creates no trace; the spec
// dispatcher's traced dispatch goes through sling.Run, whose deps carry no
// IsRigName. getRigGit returns a bare-repo handle its spawn-path caller
// observes before the first call (detectSpawnIntegrationBranchTo), while its
// other callers run untraced.
var spawnPathGitSites = map[string]spawnGitSite{
	"openSlingRig": {count: 2, observed: true},
	"startSession": {count: 2, observed: true},
	"IsRigName": {count: 1, reason: "resolves a `gt sling` command-line target (slingRun's isRigName " +
		"and workflowTargetOverride), and that command path creates no trace; the spec dispatcher's " +
		"traced dispatch goes through sling.Run, whose deps carry no IsRigName"},
	"getRigGit": {count: 1, reason: "the spawn path's caller observes the returned handle; " +
		"its other callers (polecat, batch, start) run with no trace"},
}

// minSpawnGitConstructions is the floor the git guard's scan must find before
// it trusts itself; polecat_spawn.go builds more, and a scan that sees fewer
// has stopped seeing the file.
const minSpawnGitConstructions = 5

// minSpawnManagerSites is the floor the manager guard's scan must find.
const minSpawnManagerSites = 4

// spawnManagerSite is what one function in polecat_spawn.go does with the
// polecat Manager and SessionManager it builds: how many, and whether each
// build takes the observer as a construction argument.
type spawnManagerSite struct {
	count    int    // manager builds in the function
	observed bool   // each build takes the observer at construction
	reason   string // why an untraced build takes none
}

// spawnPathManagerSites names every polecat Manager and SessionManager
// polecat_spawn.go builds. The two the sling builds (openSlingRig, startSession)
// take the trace's counter at construction, so their git handles count on the
// step that made them without a later mutation of the handle (gt-74tzuk). The
// other two (realIdleReuseEnv, prepareSlingPolecat) read only a session name
// and open no git handle.
var spawnPathManagerSites = map[string]spawnManagerSite{
	"openSlingRig":        {count: 1, observed: true},
	"startSession":        {count: 2, observed: true},
	"realIdleReuseEnv":    {count: 1, reason: "reads the session name and opens no git handle"},
	"prepareSlingPolecat": {count: 1, reason: "reads the session name and opens no git handle"},
}

// TestSlingCallsSpawnPathGitHandlesCarryTheObserver is the box-3 guard: every
// git handle the spawn path builds is observed or named with its reason, so an
// unobserved construction cannot slip in and make a sling's note line undercount
// the git calls a step made (gt-74tzuk).
func TestSlingCallsSpawnPathGitHandlesCarryTheObserver(t *testing.T) {
	t.Parallel()
	for _, problem := range spawnPathGitProblems(spawnFileGitConstructions(t), spawnPathGitSites) {
		t.Error(problem)
	}
}

// spawnPathGitProblems returns one problem per way the scanned git
// constructions in polecat_spawn.go deviate from the guard table.
func spawnPathGitProblems(found map[string]spawnBuildCounts, sites map[string]spawnGitSite) []string {
	total := 0
	for _, site := range found {
		total += site.observed + site.unobserved
	}
	if total < minSpawnGitConstructions {
		return []string{fmt.Sprintf("found %d git constructions in polecat_spawn.go, want at least %d: the scanner has stopped seeing them", total, minSpawnGitConstructions)}
	}
	var problems []string
	for _, name := range spawnFileSortedKeys(found) {
		site := found[name]
		want, known := sites[name]
		if !known {
			problems = append(problems, fmt.Sprintf("%s builds %d git handle(s); name it in spawnPathGitSites with the observer it carries or the reason it carries none", name, site.observed+site.unobserved))
			continue
		}
		if got := site.observed + site.unobserved; got != want.count {
			problems = append(problems, fmt.Sprintf("%s builds %d git handle(s), spawnPathGitSites says %d", name, got, want.count))
		}
		if want.observed && site.unobserved > 0 {
			problems = append(problems, fmt.Sprintf("%s builds %d git handle(s) without .WithRunObserver; the sling's git calls go uncounted there", name, site.unobserved))
		}
		if !want.observed && (site.observed > 0 || want.reason == "") {
			problems = append(problems, fmt.Sprintf("%s is listed as unobserved but %d handle(s) carry an observer or no reason is given; observe them and delete the entry, or state the reason", name, site.observed))
		}
	}
	for _, name := range spawnFileSortedKeys(sites) {
		if _, ok := found[name]; !ok {
			problems = append(problems, fmt.Sprintf("spawnPathGitSites names %s, which no longer builds a git handle; delete the entry", name))
		}
	}
	return problems
}

// TestSlingCallsSpawnPathManagersTakeTheObserver fails when the spawn path
// builds a polecat Manager or SessionManager without passing the trace's
// counter at construction, or when a new build site appears unclassified.
func TestSlingCallsSpawnPathManagersTakeTheObserver(t *testing.T) {
	t.Parallel()
	for _, problem := range spawnPathManagerProblems(spawnFileManagerBuilds(t), spawnPathManagerSites) {
		t.Error(problem)
	}
}

// spawnPathManagerProblems returns one problem per way the scanned manager
// builds in polecat_spawn.go deviate from the guard table.
func spawnPathManagerProblems(found map[string]spawnBuildCounts, sites map[string]spawnManagerSite) []string {
	if len(found) < minSpawnManagerSites {
		return []string{fmt.Sprintf("found %d manager build sites in polecat_spawn.go, want at least %d: the scanner has stopped seeing them", len(found), minSpawnManagerSites)}
	}
	var problems []string
	for _, name := range spawnFileSortedKeys(found) {
		site := found[name]
		want, known := sites[name]
		if !known {
			problems = append(problems, fmt.Sprintf("%s builds %d manager(s); name it in spawnPathManagerSites with the observer it takes or the reason it takes none", name, site.observed+site.unobserved))
			continue
		}
		if got := site.observed + site.unobserved; got != want.count {
			problems = append(problems, fmt.Sprintf("%s builds %d manager(s), spawnPathManagerSites says %d", name, got, want.count))
		}
		if want.observed && site.unobserved > 0 {
			problems = append(problems, fmt.Sprintf("%s builds %d manager(s) without the trace's observer at construction; their git calls go uncounted there", name, site.unobserved))
		}
		if !want.observed && (site.observed > 0 || want.reason == "") {
			problems = append(problems, fmt.Sprintf("%s is listed as untraced but %d build(s) take an observer or no reason is given; delete the entry, or state the reason", name, site.observed))
		}
	}
	for _, name := range spawnFileSortedKeys(sites) {
		if _, ok := found[name]; !ok {
			problems = append(problems, fmt.Sprintf("spawnPathManagerSites names %s, which no longer builds a manager; delete the entry", name))
		}
	}
	return problems
}

// TestSlingCallsSpawnPathGuardsRefuseEachBreach drives every failing branch of
// the two box-3 guards with a fabricated scan, so each refusal is exercised by
// a test and not only by the file the guard happens to scan today (gt-74tzuk).
func TestSlingCallsSpawnPathGuardsRefuseEachBreach(t *testing.T) {
	t.Parallel()
	// The clean rows the breach rows start from: each names every site its
	// guard table expects, so only the branch under test can fire.
	cleanGitFound := map[string]spawnBuildCounts{
		"openSlingRig": {observed: 2},
		"startSession": {observed: 2},
		"IsRigName":    {unobserved: 1},
	}
	cleanGitSites := map[string]spawnGitSite{
		"openSlingRig": {count: 2, observed: true},
		"startSession": {count: 2, observed: true},
		"IsRigName":    {count: 1, reason: "no trace on its path"},
	}
	cleanManagerFound := map[string]spawnBuildCounts{
		"openSlingRig":        {observed: 1},
		"startSession":        {observed: 2},
		"realIdleReuseEnv":    {unobserved: 1},
		"prepareSlingPolecat": {unobserved: 1},
	}
	cleanManagerSites := map[string]spawnManagerSite{
		"openSlingRig":        {count: 1, observed: true},
		"startSession":        {count: 2, observed: true},
		"realIdleReuseEnv":    {count: 1, reason: "opens no git handle"},
		"prepareSlingPolecat": {count: 1, reason: "opens no git handle"},
	}

	git := func(found map[string]spawnBuildCounts, sites map[string]spawnGitSite) func() []string {
		return func() []string { return spawnPathGitProblems(found, sites) }
	}
	manager := func(found map[string]spawnBuildCounts, sites map[string]spawnManagerSite) func() []string {
		return func() []string { return spawnPathManagerProblems(found, sites) }
	}

	tests := []struct {
		name  string
		check func() []string
		want  string // a substring of the one problem the check must return; "" means none
	}{
		{
			name:  "git scan stops seeing the file",
			check: git(map[string]spawnBuildCounts{}, cleanGitSites),
			want:  "want at least 5",
		},
		{
			name: "git site is unclassified",
			check: git(map[string]spawnBuildCounts{
				"openSlingRig": {observed: 2},
				"startSession": {observed: 2},
				"IsRigName":    {unobserved: 1},
				"newSite":      {observed: 1},
			}, cleanGitSites),
			want: "newSite builds 1 git handle(s); name it in spawnPathGitSites",
		},
		{
			name: "git site count drifts",
			check: git(map[string]spawnBuildCounts{
				"openSlingRig": {observed: 3},
				"startSession": {observed: 2},
				"IsRigName":    {unobserved: 1},
			}, cleanGitSites),
			want: "openSlingRig builds 3 git handle(s), spawnPathGitSites says 2",
		},
		{
			name: "observed git site loses its observer",
			check: git(map[string]spawnBuildCounts{
				"openSlingRig": {unobserved: 2},
				"startSession": {observed: 2},
				"IsRigName":    {unobserved: 1},
			}, cleanGitSites),
			want: "openSlingRig builds 2 git handle(s) without .WithRunObserver",
		},
		{
			name: "unobserved git site carries an observer",
			check: git(map[string]spawnBuildCounts{
				"openSlingRig": {observed: 2},
				"startSession": {observed: 2},
				"IsRigName":    {observed: 1},
			}, cleanGitSites),
			want: "IsRigName is listed as unobserved but 1 handle(s) carry an observer",
		},
		{
			name: "unobserved git site states no reason",
			check: git(cleanGitFound, map[string]spawnGitSite{
				"openSlingRig": {count: 2, observed: true},
				"startSession": {count: 2, observed: true},
				"IsRigName":    {count: 1},
			}),
			want: "IsRigName is listed as unobserved but 0 handle(s) carry an observer or no reason is given",
		},
		{
			name: "git table names a site the scan no longer finds",
			check: git(cleanGitFound, map[string]spawnGitSite{
				"openSlingRig": {count: 2, observed: true},
				"startSession": {count: 2, observed: true},
				"IsRigName":    {count: 1, reason: "no trace on its path"},
				"getRigGit":    {count: 1, reason: "its caller observes it"},
			}),
			want: "spawnPathGitSites names getRigGit, which no longer builds a git handle",
		},
		{
			name:  "git guard accepts a matching scan",
			check: git(cleanGitFound, cleanGitSites),
			want:  "",
		},
		{
			name:  "manager scan stops seeing the file",
			check: manager(map[string]spawnBuildCounts{"openSlingRig": {observed: 1}}, cleanManagerSites),
			want:  "want at least 4",
		},
		{
			name: "manager site is unclassified",
			check: manager(map[string]spawnBuildCounts{
				"openSlingRig":        {observed: 1},
				"startSession":        {observed: 2},
				"realIdleReuseEnv":    {unobserved: 1},
				"prepareSlingPolecat": {unobserved: 1},
				"newSite":             {observed: 1},
			}, cleanManagerSites),
			want: "newSite builds 1 manager(s); name it in spawnPathManagerSites",
		},
		{
			name: "manager site count drifts",
			check: manager(map[string]spawnBuildCounts{
				"openSlingRig":        {observed: 2},
				"startSession":        {observed: 2},
				"realIdleReuseEnv":    {unobserved: 1},
				"prepareSlingPolecat": {unobserved: 1},
			}, cleanManagerSites),
			want: "openSlingRig builds 2 manager(s), spawnPathManagerSites says 1",
		},
		{
			name: "observed manager site loses the observer",
			check: manager(map[string]spawnBuildCounts{
				"openSlingRig":        {unobserved: 1},
				"startSession":        {observed: 2},
				"realIdleReuseEnv":    {unobserved: 1},
				"prepareSlingPolecat": {unobserved: 1},
			}, cleanManagerSites),
			want: "openSlingRig builds 1 manager(s) without the trace's observer at construction",
		},
		{
			name: "untraced manager site takes an observer",
			check: manager(map[string]spawnBuildCounts{
				"openSlingRig":        {observed: 1},
				"startSession":        {observed: 2},
				"realIdleReuseEnv":    {observed: 1},
				"prepareSlingPolecat": {unobserved: 1},
			}, cleanManagerSites),
			want: "realIdleReuseEnv is listed as untraced but 1 build(s) take an observer",
		},
		{
			name: "untraced manager site states no reason",
			check: manager(cleanManagerFound, map[string]spawnManagerSite{
				"openSlingRig":        {count: 1, observed: true},
				"startSession":        {count: 2, observed: true},
				"realIdleReuseEnv":    {count: 1},
				"prepareSlingPolecat": {count: 1, reason: "opens no git handle"},
			}),
			want: "realIdleReuseEnv is listed as untraced but 0 build(s) take an observer or no reason is given",
		},
		{
			name: "manager table names a site the scan no longer finds",
			check: manager(cleanManagerFound, map[string]spawnManagerSite{
				"openSlingRig":        {count: 1, observed: true},
				"startSession":        {count: 2, observed: true},
				"realIdleReuseEnv":    {count: 1, reason: "opens no git handle"},
				"prepareSlingPolecat": {count: 1, reason: "opens no git handle"},
				"retiredSite":         {count: 1, reason: "opens no git handle"},
			}),
			want: "spawnPathManagerSites names retiredSite, which no longer builds a manager",
		},
		{
			name:  "manager guard accepts a matching scan",
			check: manager(cleanManagerFound, cleanManagerSites),
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			problems := tt.check()
			if tt.want == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none for a scan that matches the table", problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want exactly the one breach under test", problems)
			}
			if !strings.Contains(problems[0], tt.want) {
				t.Fatalf("problem = %q, want it to hold %q", problems[0], tt.want)
			}
		})
	}
}

// TestSlingCallsClassifiersCountSyntheticSources drives the two classifiers
// that the box-3 guard trusts (gitConstructionsIn and managerBuildsIn) with
// small synthetic sources, so a classifier that always counted zero, or read a
// nil observer as observed, fails here instead of staying silent until
// polecat_spawn.go changes.
func TestSlingCallsClassifiersCountSyntheticSources(t *testing.T) {
	t.Parallel()
	gitSrc := `package p
func clean() { g := git.NewGit(d).WithRunObserver(o); _ = g }
func bare() { g := git.NewGit(d); _ = g }
func dir() { g := git.NewGitWithDir(a, b); _ = g }
func mixed() { x := git.NewGit(d).WithRunObserver(o); y := git.NewGit(e); _, _ = x, y }
func none() { _ = other.NewGit(d) }
`
	wantGit := map[string]spawnBuildCounts{
		"clean": {observed: 1},
		"bare":  {unobserved: 1},
		"dir":   {unobserved: 1},
		"mixed": {observed: 1, unobserved: 1},
	}
	if got := gitConstructionsIn(parseSpawnSource(t, gitSrc)); !reflect.DeepEqual(got, wantGit) {
		t.Errorf("gitConstructionsIn = %+v, want %+v", got, wantGit)
	}
	mgrSrc := `package p
func clean() { _ = polecat.NewManagerWithObserver(r, o); _ = polecat.NewSessionManagerWithObserver(r, o) }
func nilObserver() { _ = polecat.NewManagerWithObserver(r, nil) }
func nilSession() { _ = polecat.NewSessionManagerWithObserver(r, nil) }
func plain() { _ = polecat.NewManager(r); _ = polecat.NewSessionManager(r) }
func supervised() { _ = supervisedPolecatManager(r, o); _ = supervisedPolecatManager(r, nil) }
func none() { _ = other.NewManager(r) }
`
	wantMgr := map[string]spawnBuildCounts{
		"clean":       {observed: 2},
		"nilObserver": {unobserved: 1},
		"nilSession":  {unobserved: 1},
		"plain":       {unobserved: 2},
		"supervised":  {observed: 1, unobserved: 1},
	}
	if got := managerBuildsIn(parseSpawnSource(t, mgrSrc)); !reflect.DeepEqual(got, wantMgr) {
		t.Errorf("managerBuildsIn = %+v, want %+v", got, wantMgr)
	}
}

// spawnFileGitConstructions parses polecat_spawn.go and counts, per function,
// the git handles it builds and whether each construction statement observes
// them with .WithRunObserver.
func spawnFileGitConstructions(t *testing.T) map[string]spawnBuildCounts {
	t.Helper()
	return gitConstructionsIn(parseSpawnFile(t))
}

// gitConstructionsIn is spawnFileGitConstructions over an already parsed file,
// so a test can feed the classifier a synthetic source.
func gitConstructionsIn(fn *ast.File) map[string]spawnBuildCounts {
	found := map[string]spawnBuildCounts{}
	for name, body := range spawnFileFunctions(fn) {
		observed := map[token.Pos]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if inner, ok := observedGitConstruction(call); ok {
					observed[inner.Pos()] = true
				}
			}
			return true
		})
		site := spawnBuildCounts{}
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isGitConstruction(call) {
				return true
			}
			if observed[call.Pos()] {
				site.observed++
			} else {
				site.unobserved++
			}
			return true
		})
		if site.observed+site.unobserved > 0 {
			found[name] = site
		}
	}
	return found
}

// spawnFileManagerBuilds parses polecat_spawn.go and reports, per function,
// whether it builds a polecat Manager or SessionManager with an observer
// argument at construction. A build whose last argument is the literal nil, or
// a constructor without an observer of its own, does not count as observed.
func spawnFileManagerBuilds(t *testing.T) map[string]spawnBuildCounts {
	t.Helper()
	return managerBuildsIn(parseSpawnFile(t))
}

// managerBuildsIn is spawnFileManagerBuilds over an already parsed file, so a
// test can feed the classifier a synthetic source.
func managerBuildsIn(fn *ast.File) map[string]spawnBuildCounts {
	found := map[string]spawnBuildCounts{}
	for name, body := range spawnFileFunctions(fn) {
		site := spawnBuildCounts{}
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			observed, isBuild := managerBuildCall(call)
			if !isBuild {
				return true
			}
			if observed {
				site.observed++
			} else {
				site.unobserved++
			}
			return true
		})
		if site.observed+site.unobserved > 0 {
			found[name] = site
		}
	}
	return found
}

// managerBuildCall reports whether call builds a polecat Manager or
// SessionManager, and whether the build takes the observer at construction:
// the WithObserver constructors and supervisedPolecatManager do, and a build
// passing a literal nil does not count.
func managerBuildCall(call *ast.CallExpr) (observed, isBuild bool) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if fun.Name == "supervisedPolecatManager" {
			return !lastArgIsNil(call), true
		}
	case *ast.SelectorExpr:
		pkg, ok := fun.X.(*ast.Ident)
		if !ok || pkg.Name != "polecat" {
			return false, false
		}
		switch fun.Sel.Name {
		case "NewManagerWithObserver", "NewSessionManagerWithObserver":
			return !lastArgIsNil(call), true
		case "NewManager", "NewManagerWithStore", "NewSessionManager":
			return false, true
		}
	}
	return false, false
}

// lastArgIsNil reports whether call passes the nil identifier as its last
// argument.
func lastArgIsNil(call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	id, ok := call.Args[len(call.Args)-1].(*ast.Ident)
	return ok && id.Name == "nil"
}

// isGitConstruction reports whether call is git.NewGit(...) or
// git.NewGitWithDir(...).
func isGitConstruction(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "NewGit" && sel.Sel.Name != "NewGitWithDir" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "git"
}

// observedGitConstruction reports whether call is x.WithRunObserver(...) whose
// receiver x is a git construction in the same statement, and returns x.
func observedGitConstruction(call *ast.CallExpr) (*ast.CallExpr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "WithRunObserver" {
		return nil, false
	}
	inner, ok := sel.X.(*ast.CallExpr)
	if !ok || !isGitConstruction(inner) {
		return nil, false
	}
	return inner, true
}

// parseSpawnSource parses a synthetic Go source for the classifier tests.
func parseSpawnSource(t *testing.T, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	return f
}

// parseSpawnFile parses the spawn file beside this test.
func parseSpawnFile(t *testing.T) *ast.File {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "polecat_spawn.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return f
}

// spawnFileFunctions maps each function in f to its body.
func spawnFileFunctions(f *ast.File) map[string]*ast.BlockStmt {
	fns := map[string]*ast.BlockStmt{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		fns[fn.Name.Name] = fn.Body
	}
	return fns
}

// spawnFileSortedKeys returns m's keys in order, so failures read the same
// every run.
func spawnFileSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
