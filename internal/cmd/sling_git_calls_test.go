package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
// CLI rig target in a helper that no traced path reaches — the spec dispatcher
// resolves its target inside sling.Run, which does not call it — and getRigGit
// returns a bare-repo handle its spawn-path caller observes before the first
// call (detectSpawnIntegrationBranchTo), while its other callers run untraced.
var spawnPathGitSites = map[string]spawnGitSite{
	"openSlingRig": {count: 2, observed: true},
	"startSession": {count: 2, observed: true},
	"IsRigName": {count: 1, reason: "resolves a CLI rig target, and no trace exists on any path " +
		"that reaches it: the spec dispatcher resolves its target inside sling.Run"},
	"getRigGit": {count: 1, reason: "the spawn path's caller observes the returned handle; " +
		"its other callers (polecat, batch, start) run with no trace"},
}

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
	found := spawnFileGitConstructions(t)
	total := 0
	for _, site := range found {
		total += site.observed + site.unobserved
	}
	if total < 5 {
		t.Fatalf("found %d git constructions in polecat_spawn.go, want at least 5: the scanner has stopped seeing them", total)
	}
	for _, name := range spawnFileSortedKeys(found) {
		site := found[name]
		want, known := spawnPathGitSites[name]
		if !known {
			t.Errorf("%s builds %d git handle(s); name it in spawnPathGitSites with the observer it carries or the reason it carries none", name, site.observed+site.unobserved)
			continue
		}
		if got := site.observed + site.unobserved; got != want.count {
			t.Errorf("%s builds %d git handle(s), spawnPathGitSites says %d", name, got, want.count)
		}
		if want.observed && site.unobserved > 0 {
			t.Errorf("%s builds %d git handle(s) without .WithRunObserver; the sling's git calls go uncounted there", name, site.unobserved)
		}
		if !want.observed && (site.observed > 0 || want.reason == "") {
			t.Errorf("%s is listed as unobserved but %d handle(s) carry an observer or no reason is given; observe them and delete the entry, or state the reason", name, site.observed)
		}
	}
	for name := range spawnPathGitSites {
		if _, ok := found[name]; !ok {
			t.Errorf("spawnPathGitSites names %s, which no longer builds a git handle; delete the entry", name)
		}
	}
}

// TestSlingCallsSpawnPathManagersTakeTheObserver fails when the spawn path
// builds a polecat Manager or SessionManager without passing the trace's
// counter at construction, or when a new build site appears unclassified.
func TestSlingCallsSpawnPathManagersTakeTheObserver(t *testing.T) {
	t.Parallel()
	found := spawnFileManagerBuilds(t)
	if len(found) < 4 {
		t.Fatalf("found %d manager build sites in polecat_spawn.go, want at least 4: the scanner has stopped seeing them", len(found))
	}
	for _, name := range spawnFileSortedKeys(found) {
		site := found[name]
		want, known := spawnPathManagerSites[name]
		if !known {
			t.Errorf("%s builds %d manager(s); name it in spawnPathManagerSites with the observer it takes or the reason it takes none", name, site.observed+site.unobserved)
			continue
		}
		if got := site.observed + site.unobserved; got != want.count {
			t.Errorf("%s builds %d manager(s), spawnPathManagerSites says %d", name, got, want.count)
		}
		if want.observed && site.unobserved > 0 {
			t.Errorf("%s builds %d manager(s) without the trace's observer at construction; their git calls go uncounted there", name, site.unobserved)
		}
		if !want.observed && (site.observed > 0 || want.reason == "") {
			t.Errorf("%s is listed as untraced but %d build(s) take an observer or no reason is given; delete the entry, or state the reason", name, site.observed)
		}
	}
	for name := range spawnPathManagerSites {
		if _, ok := found[name]; !ok {
			t.Errorf("spawnPathManagerSites names %s, which no longer builds a manager; delete the entry", name)
		}
	}
}

// spawnFileGitConstructions parses polecat_spawn.go and counts, per function,
// the git handles it builds and whether each construction statement observes
// them with .WithRunObserver.
func spawnFileGitConstructions(t *testing.T) map[string]spawnBuildCounts {
	t.Helper()
	fn := parseSpawnFile(t)
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
	fn := parseSpawnFile(t)
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
