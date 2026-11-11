package testutil

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// parallelGlobalWaiver is the comment marker that excuses a parallel test from
// this guard. Put it in the test's own comment block, with the reason:
//
//	//parallel-global-ok: the flag this swaps is read only by the code under
//	//test in this same goroutine, never by a peer test.
//
// The reason is the point. A waiver with no reason is a silent reintroduction
// of gt-k317, and reviewers should reject it the way they reject a bare
// //nolint.
const parallelGlobalWaiver = "parallel-global-ok"

// TestNoParallelTestsReachProcessGlobalSwaps is the gt-k317 structural guard.
//
// A test helper that swaps a process global and restores it in t.Cleanup is
// unusable from a t.Parallel test: two parallel callers save each other's
// value and the last cleanup to run wins, so whichever global the loser
// installed stays up for everything that follows. gt-fo3h hit this as a live
// failure — TestSessionWorkDir resolved "cannot determine prefix" because a
// neighbouring parallel test's registry restore had dropped the gt prefix —
// and could only fix it by leaving 21 tests serial by hand.
//
// Serial-by-hand is unenforceable: the next t.Parallel() added to any of those
// tests silently reintroduces the race, and the resulting failure lands in a
// different test than the one that broke the invariant. This test is the
// enforceable version. It reads the shape instead of the intent — a parallel
// test that reaches, through any chain of helpers in its own package, a write
// to a process global — and fails with the call chain that reaches it.
//
// Two ways out, both fine: keep the test sequential, or make its seam
// install-once and never restore (TestMain, sync.Once), which is what
// internal/refinery's TestMain does and what makes t.Parallel safe there.
//
// The shapes it reads: a write to a package variable (an assignment, or a
// Swap, Store or CompareAndSwap with no key argument) that is restored or takes
// a caller's value; a reset-named helper (Reset*, Clear*, Invalidate*, *AndReset)
// that zeroes, clears or deletes a package variable; a call into another
// package's seam; and a method call, matched by name and argument count to a
// method of this package that writes a package variable.
//
// Residual, not seen: state a parallel test shares through a fixed filesystem
// path, a port, the environment, the working directory, or a reset reached
// through another package's exported function. Method calls are unresolved:
// a call is matched by name and argument count to a direct writer, so a method
// that writes a package variable only through a further call is not followed.
// Each such chain is logged by name as unfollowed in the test log, and is not
// a failure. The guard reads package variables and helper calls only, with no
// type information, so a clean verdict does not prove a parallel test safe on
// those.
func TestNoParallelTestsReachProcessGlobalSwaps(t *testing.T) {
	t.Parallel()
	scanner := newSeamScanner(sharedRepoTree(t))

	violations := scanner.scan(t)
	if len(violations) == 0 {
		return
	}
	t.Fatalf("a t.Parallel test must not reach a process-global swap. Either drop the\n"+
		"t.Parallel call, install the global once for the whole package (TestMain,\n"+
		"sync.Once) so no test restores it, or excuse this test in its own comment\n"+
		"block with a reason:\n\n"+
		"    //%s: <why a parallel caller is safe here>\n\n"+
		"%s", parallelGlobalWaiver, strings.Join(violations, "\n"))
}

// TestParallelGlobalGuardSeesItsTarget is the guard's own regression test. A
// scanner this narrow — direct writes, free-function calls, a package's own
// globals — fails by going blind, not by crashing: tighten one rule too far and
// it reports zero violations forever, which is indistinguishable from a clean
// tree. So it is run over a fixture whose verdict is known, covering each way a
// swap can be safe or unsafe.
func TestParallelGlobalGuardSeesItsTarget(t *testing.T) {
	t.Parallel()
	fixture, err := loadRepoTree(writeGuardFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	scanner := newSeamScanner(fixture)
	got := scanner.scan(t)

	// Test names, not line numbers: the fixture is edited more often than the
	// rule, and a shifted line would turn every case into a false alarm.
	want := map[string]bool{ // test name -> should be reported
		"TestParallelSwapsSeam":      true,  // parallel, calls a helper that installs a seam
		"TestParallelTwoHelpersDeep": true,  // parallel, reaches the seam two helpers deep
		"TestParallelWritesStdout":   true,  // parallel, reassigns os.Stdout
		"TestSequentialSwapsSeam":    false, // sequential: the supported answer
		"TestParallelWaived":         false, // parallel, waived with a reason
		"TestParallelInstallsOnce":   false, // parallel, installs once and never restores
		"TestParallelOnlyReads":      false, // parallel, only reads the seam
		// The witness race (nudgeRefinery): a parallel test swaps the global
		// in its own body and restores it with defer rather than t.Cleanup.
		"TestParallelDeferRestores":    true,
		"TestParallelDeferArgRestores": true,  // defer func(v int) { Seam = v }(Seam)
		"TestParallelDeferUnlocksOnly": false, // a defer that restores nothing is no swap
		// A named result that shares its name with a package-level func or
		// method is a local: writing it is no global write (beads allowStaleAnswer).
		"TestParallelNamedResultShadows": false,
		// `var _ I = (*T)(nil)` declares no variable, so `_ = x` in a parallel
		// test writes nothing global.
		"TestParallelBlankAssignment": false,
		// A method and a free function can share a name; they are different
		// functions. The free Install is clean even though a method named
		// Install declared before it writes the seam, and the free Poke writes
		// the seam even though a clean method named Poke is declared first.
		"TestParallelCallsFreeInstall": false,
		"TestParallelCallsFreePoke":    true,
		// The same collision across packages: other.Test calls pkg.Store and
		// pkg.Nudge, where only the free functions are what the call names.
		"TestParallelCallsOtherStore": false,
		"TestParallelCallsOtherNudge": true,
		// Shape 1: a package-level atomic value swapped, then restored in
		// t.Cleanup. The sequential and waived twins are the supported answers.
		"TestParallelSwapsAtomicSlot":   true,
		"TestSequentialSwapsAtomicSlot": false,
		"TestParallelWaivedAtomicSlot":  false,
		// Shape 2: a reset that empties a package map and counter, named as a
		// reset, with no parameter and no restore.
		"TestParallelResetsCensus":       true,
		"TestSequentialResetsCensus":     false,
		"TestParallelWaivedResetsCensus": false,
		// A delete keyed by a parameter is a per-key write unless the function
		// is named as a reset, which Invalidate is.
		"TestParallelInvalidatesKey": true,
		// A lazy cache fill in a function not named as a reset is no seam.
		"TestParallelLazyFill": false,
		// A parallel test that only increments counters writes no seam.
		"TestParallelIncrementsCounter": false,
		// A sync.Map keyed by a per-test value is isolated: its Store takes a key
		// and a value, so the unkeyed rule does not match it, and the argument
		// count rules out Box.Store, the same-named writer in this package.
		"TestParallelKeysSyncMap": false,
		// A method call that the guard resolves by name and argument count to a
		// writer method of this package is reported by name.
		"TestParallelCallsResetMethod": true,
		// A bare Swap with no restore is a setter, not a seam (the spec's shape
		// restores what it swapped).
		"TestParallelSetsSlotNoRestore": false,
		// An environment writer reached by a method name is out of scope.
		"TestParallelCallsEnvSetter": false,
		// A method that writes only through a further call is not followed: the
		// test is not reported, and the log names its chain (checked below).
		"TestParallelCallsRelayForward": false,
		// A reset in another package is the residual, not a seam.
		"TestParallelCallsOtherReset": false,
	}
	seen := map[string]bool{}
	sawParseError := false
	for _, line := range got {
		// A file this package cannot parse is reported by its path, not as a
		// test. The fixture's broken package must be among the reports.
		if strings.Contains(line, "broken"+string(filepath.Separator)+"broken.go: ") {
			sawParseError = true
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "Test") {
			t.Errorf("unparseable violation %q", line)
			continue
		}
		name := fields[1]
		seen[name] = true
		if _, known := want[name]; !known {
			t.Errorf("scanner reported %s, which the fixture does not list:\n%s", name, line)
		} else if !want[name] {
			t.Errorf("scanner reported %s, but the fixture marks it safe:\n%s", name, line)
		}
	}
	if !sawParseError {
		t.Errorf("scanner did not report the fixture's unparseable package; a package it cannot read must not read as clean.\nScanned %d violation(s):\n%s",
			len(got), strings.Join(got, "\n"))
	}
	for name, expect := range want {
		if expect && !seen[name] {
			t.Errorf("scanner missed %s; the fixture expects it reported.\nScanned %d violation(s):\n%s",
				name, len(got), strings.Join(got, "\n"))
		}
	}
	// The unfollowed method call is not a violation, but it must be named by
	// test and method in the log, never read as clean in silence.
	var sawUnfollowed bool
	for _, line := range scanner.unfollowed {
		if strings.Contains(line, " TestParallelCallsRelayForward — unfollowed: reaches method relay.Forward at ") {
			sawUnfollowed = true
		}
	}
	if !sawUnfollowed {
		t.Errorf("scanner did not log the unfollowed method call relay.Forward reached from TestParallelCallsRelayForward.\nUnfollowed:\n%s",
			strings.Join(scanner.unfollowed, "\n"))
	}
}

// writeGuardFixture lays down a throwaway module with one file whose tests
// cover every branch of the scanner, and returns its root.
func writeGuardFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.test\n\ngo 1.21\n",
		// A global and the two ways test code writes it: a setter that takes
		// the new value, and a direct assignment.
		"pkg/globals.go": `package pkg

var Seam = 1

// SetSeam installs a value process-wide.
func SetSeam(v int) { Seam = v }

// ReadSeam reads it.
func ReadSeam() int { return Seam }

// Store and Nudge each exist twice: as a method and as a free function. Only
// the method Store and the free Nudge write the seam.
type Box struct{}

func (Box) Store(v int) { Seam = v }

func Store() {}

type Nudger struct{}

func (Nudger) Nudge() {}

func Nudge(v int) { Seam = v }

// Holder's Reset writes the seam from its argument. A parallel test calling
// h.Reset(3) reaches it by name and argument count, with no type information.
type Holder struct{}

func (Holder) Reset(v int) { Seam = v }
`,
		// A package the parser cannot read, with no test file. Its seam would be
		// invisible to every package that calls it, so the guard reports it by
		// path rather than read the package as clean.
		"broken/broken.go": "package broken\n\nfunc (\n",
		"other/other_test.go": `package other

import (
	"testing"

	"example.test/pkg"
)

func TestParallelCallsOtherStore(t *testing.T) {
	t.Parallel()
	pkg.Store()
}

func TestParallelCallsOtherNudge(t *testing.T) {
	t.Parallel()
	pkg.Nudge(8)
}

// A reset in another package is the residual: the guard reads a package's own
// resets, not an exported one a caller in another package reaches.
func TestParallelCallsOtherReset(t *testing.T) {
	t.Parallel()
	pkg.ResetCensus()
}
`,
		"pkg/seam_test.go": `package pkg

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func swapSeam(t *testing.T) {
	old := Seam
	SetSeam(2)
	t.Cleanup(func() { SetSeam(old) })
}

func TestParallelSwapsSeam(t *testing.T) {
	t.Parallel()
	swapSeam(t)
}

func TestSequentialSwapsSeam(t *testing.T) {
	swapSeam(t)
}

//parallel-global-ok: read only by this test's own goroutine.
func TestParallelWaived(t *testing.T) {
	t.Parallel()
	SetSeam(3)
}

func TestParallelTwoHelpersDeep(t *testing.T) {
	t.Parallel()
	outerHelper(t)
}

func outerHelper(t *testing.T) { swapSeam(t) }

func TestParallelWritesStdout(t *testing.T) {
	t.Parallel()
	old := os.Stdout
	os.Stdout = nil
	t.Cleanup(func() { os.Stdout = old })
}

var once sync.Once

func installOnce() { once.Do(func() { SetSeam(4) }) }

func TestParallelInstallsOnce(t *testing.T) {
	t.Parallel()
	installOnce()
}

func TestParallelOnlyReads(t *testing.T) {
	t.Parallel()
	_ = ReadSeam()
}

func TestParallelDeferRestores(t *testing.T) {
	t.Parallel()
	old := Seam
	defer func() { Seam = old }()
	Seam = 5
}

func TestParallelDeferArgRestores(t *testing.T) {
	t.Parallel()
	defer func(v int) { Seam = v }(Seam)
	Seam = 6
}

var mu sync.Mutex

func bumpLocked() {
	mu.Lock()
	defer mu.Unlock()
	Seam = Seam + 1
}

func TestParallelDeferUnlocksOnly(t *testing.T) {
	t.Parallel()
	bumpLocked()
}

type cache struct{}

func (cache) answer() bool { return true }

func classify(n int) (answer bool, sure bool) {
	answer = n > 0
	sure = true
	return answer, sure
}

func TestParallelNamedResultShadows(t *testing.T) {
	t.Parallel()
	_, _ = classify(1)
}

type reader interface{ read() int }

type seamReader struct{}

func (seamReader) read() int { return Seam }

var _ reader = seamReader{}

func TestParallelBlankAssignment(t *testing.T) {
	t.Parallel()
	_ = seamReader{}.read()
	t.Cleanup(func() { _ = seamReader{}.read() })
}

type installer struct{}

func (installer) Install(v int) { Seam = v }

func Install() {}

func TestParallelCallsFreeInstall(t *testing.T) {
	t.Parallel()
	Install()
}

type poker struct{}

func (*poker) Poke() {}

func Poke(v int) { Seam = v }

func TestParallelCallsFreePoke(t *testing.T) {
	t.Parallel()
	Poke(7)
}

// Shape 1: a package-level atomic value swapped, then restored in t.Cleanup.
var slot atomic.Pointer[int]

func useSlot(t *testing.T) {
	s := new(int)
	prev := slot.Swap(s)
	t.Cleanup(func() { slot.Store(prev) })
}

func TestParallelSwapsAtomicSlot(t *testing.T) {
	t.Parallel()
	useSlot(t)
}

func TestSequentialSwapsAtomicSlot(t *testing.T) {
	useSlot(t)
}

//parallel-global-ok: the slot is read only by this test's own goroutine.
func TestParallelWaivedAtomicSlot(t *testing.T) {
	t.Parallel()
	useSlot(t)
}

// Shape 2: a reset that empties process state. It has no parameter and no
// restore, and is a seam because it is named as a reset and clears or zeroes
// a package variable.
var census = map[string]int{}

var censusTotal int

func ResetCensus() {
	clear(census)
	censusTotal = 0
}

func TestParallelResetsCensus(t *testing.T) {
	t.Parallel()
	ResetCensus()
}

func TestSequentialResetsCensus(t *testing.T) {
	ResetCensus()
}

//parallel-global-ok: only this test's own goroutine reads the census.
func TestParallelWaivedResetsCensus(t *testing.T) {
	t.Parallel()
	ResetCensus()
}

// A delete keyed by a parameter, in a function named as a reset.
func InvalidateCensus(key string) {
	delete(census, key)
}

func TestParallelInvalidatesKey(t *testing.T) {
	t.Parallel()
	InvalidateCensus(t.Name())
}

// A lazy fill in a function not named as a reset is no seam.
var lazy map[string]int

func lazyFill() {
	if lazy == nil {
		lazy = make(map[string]int)
	}
	lazy["k"] = 1
}

func TestParallelLazyFill(t *testing.T) {
	t.Parallel()
	lazyFill()
}

// Counters: an atomic Add and a plain increment are not seams.
var hits atomic.Int64

var birds int

func TestParallelIncrementsCounter(t *testing.T) {
	t.Parallel()
	hits.Add(1)
	birds++
}

// A sync.Map keyed by a per-test value: the key is a parameter, and the Store
// takes a key and a value, so it is isolated, not shared.
var townDown sync.Map

func useTownDown(t *testing.T, root string) {
	townDown.Store(root, true)
	t.Cleanup(func() { townDown.Delete(root) })
}

func TestParallelKeysSyncMap(t *testing.T) {
	t.Parallel()
	useTownDown(t, t.TempDir())
}

// A method call by name: h.Reset(3) matches Holder.Reset, which writes the seam.
func TestParallelCallsResetMethod(t *testing.T) {
	t.Parallel()
	var h Holder
	h.Reset(3)
}

// relay.Forward writes the seam only through SetSeam, so it is no direct writer
// and the guard cannot follow a call to it past its own body. The test is not
// reported, and its chain is logged by name as unfollowed.
type relay struct{}

func (relay) Forward() { SetSeam(9) }

func TestParallelCallsRelayForward(t *testing.T) {
	t.Parallel()
	var r relay
	r.Forward()
}

// A bare Swap with a parameter and no restore is a setter, not a swap: the
// spec's shape restores what it swapped, so this is no seam here.
func setSlotNoRestore(v *int) {
	slot.Swap(v)
}

func TestParallelSetsSlotNoRestore(t *testing.T) {
	t.Parallel()
	setSlotNoRestore(new(int))
}

// A method that writes the environment is out of scope, so a parallel test that
// calls it by name is not reported.
type envHolder struct{}

func (envHolder) Setenv(key, value string) {
	os.Setenv(key, value)
}

var env envHolder

func TestParallelCallsEnvSetter(t *testing.T) {
	t.Parallel()
	env.Setenv("K", "V")
}
`,
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

// repoRoot locates the module root from this file's position. The guard has to
// read the whole tree, not just its own package, because the parallel test and
// the helper it misuses can live in any package.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// seamScanner walks the module once, then answers the only question the guard
// asks: for each t.Parallel test, is any function it can reach — in its own
// test package, or an exported test seam in another package — a writer of a
// process global?
type seamScanner struct {
	tree    *repoTree
	root    string
	module  string
	fset    *token.FileSet
	dirs    []string
	pkgs    map[string]*pkgAnalysis // dir -> analysis
	mutator map[string]map[string]bool
	// unfollowed names each method call the guard could not follow past, from a
	// parallel test it reads as clean. It is logged, not failed.
	unfollowed []string
}

func newSeamScanner(tree *repoTree) *seamScanner {
	return &seamScanner{
		tree:    tree,
		root:    tree.root,
		module:  tree.module,
		fset:    tree.fset,
		dirs:    tree.dirs,
		pkgs:    map[string]*pkgAnalysis{},
		mutator: map[string]map[string]bool{},
	}
}

// scan runs in three passes because the passes depend on each other in one
// direction. Parsing a directory cannot resolve a call into another package
// until every package's own global writes are known, so the cross-package
// edges are a second pass over the already-parsed trees rather than part of
// parsing them.
func (s *seamScanner) scan(t *testing.T) []string {
	t.Helper()
	for _, dir := range s.dirs {
		s.analysis(dir)
	}

	// Every package's functions that themselves write a global, then every
	// function's cross-package edges into one.
	//
	// The exported set is deliberately the direct writers, not everything that
	// transitively reaches one. InitRegistry installs a registry as a side
	// effect of starting up, and every test in the witness and up packages
	// reaches it; counting it as a seam would flag them all for a write that
	// installs the same value the hermetic harness already installed. A seam a
	// caller opts into is the thing this guard is about, and opting in looks
	// like SetDefaultRegistry — a direct write from an argument.
	for _, dir := range s.dirs {
		pa := s.pkgs[dir]
		if pa == nil {
			continue
		}
		// A reset is left out: the cross-package check reads a package's
		// exported seams, and a reset that empties another package's state is
		// the residual, not a seam a caller opts into. The same package's own
		// tests still see it through pa.direct.
		mut := map[string]bool{}
		for name, w := range pa.direct {
			if !w.reset {
				mut[name] = true
			}
		}
		if len(mut) > 0 {
			s.mutator[pa.importPath] = mut
		}
	}
	for _, dir := range s.dirs {
		if pa := s.pkgs[dir]; pa != nil {
			pa.computeCrossSeams(s)
		}
	}

	var violations []string
	for _, dir := range s.dirs {
		pa := s.pkgs[dir]
		if pa == nil {
			continue
		}
		// A package with no test files has no parallel test to report, but its
		// unparsed files may hold a seam other packages call: report the parse
		// error rather than read the package as clean.
		if !pa.hasTestFiles {
			violations = append(violations, pa.parseErrors...)
			continue
		}
		violations = append(violations, pa.parallelSwaps()...)
		s.unfollowed = append(s.unfollowed, pa.unfollowedCalls()...)
	}
	sort.Strings(violations)
	sort.Strings(s.unfollowed)
	for _, line := range s.unfollowed {
		t.Logf("%s", line)
	}
	return violations
}

func (s *seamScanner) analysis(dir string) *pkgAnalysis {
	if pa, ok := s.pkgs[dir]; ok {
		return pa
	}
	pa := parseDir(s, dir)
	s.pkgs[dir] = pa
	return pa
}

// globalWrite is one direct write to process-global state, with enough context
// to name it in a failure message.
type globalWrite struct {
	pos   token.Position
	what  string
	reset bool // a reset that empties package state: not an exported seam
}

// isPackageVar reports whether a write is to a package variable, as opposed to
// an os stream or the process environment. Only package-variable writers are
// seams a method call can be matched to: the environment and working directory
// are out of this guard's scope.
func isPackageVar(what string) bool {
	return strings.HasPrefix(what, "package variable ")
}

// pkgAnalysis holds one directory's parsed package. Test and non-test files are
// merged: a parallel test reaches both its own package's helpers and the
// package's production code, and either can write a global.
type pkgAnalysis struct {
	root         string // scan root, so reported paths are relative to it
	importPath   string
	pkgLevel     map[string]bool              // package-scope names declared in this dir
	funcs        map[string]*ast.FuncDecl     // func key -> declaration
	bodyOf       map[string]*ast.BlockStmt    // func key -> body
	lineOf       map[string]int               // func key -> declaration line
	fileOf       map[string]string            // func key -> source path
	waived       map[string]bool              // func key -> carries the waiver comment
	direct       map[string]*globalWrite      // func key -> its own global write
	crossSeam    map[string][]globalWrite     // func key -> global writes via other packages
	imports      map[string]map[string]bool   // file path -> alias -> is-our-module
	crossTargets map[string]map[string]string // file path -> alias -> import path
	parseErrors  []string
	hasTestFiles bool
	writerSet    map[string]bool     // memo of writers(), built on first use
	unfollowSet  map[string]bool     // memo of unfollowReach(), built on first use
	indirect     map[string][]string // memo of indirectMethods(), built on first use
	fset         *token.FileSet
}

func parseDir(s *seamScanner, dir string) *pkgAnalysis {
	pa := &pkgAnalysis{
		root:         s.root,
		fset:         s.fset,
		importPath:   s.importPath(dir),
		pkgLevel:     map[string]bool{},
		funcs:        map[string]*ast.FuncDecl{},
		bodyOf:       map[string]*ast.BlockStmt{},
		lineOf:       map[string]int{},
		fileOf:       map[string]string{},
		waived:       map[string]bool{},
		direct:       map[string]*globalWrite{},
		crossSeam:    map[string][]globalWrite{},
		imports:      map[string]map[string]bool{},
		crossTargets: map[string]map[string]string{},
	}
	var files []*ast.File
	for _, rf := range s.tree.files[dir] {
		name, f := rf.name, rf.ast
		if rf.err != nil {
			// A file this package cannot parse is this guard's problem to
			// report, not to skip: silently ignoring it would let a swap hide
			// behind a syntax error.
			pa.parseErrors = append(pa.parseErrors, rf.path+": "+rf.err.Error())
			continue
		}
		files = append(files, f)
		if strings.HasSuffix(name, "_test.go") {
			pa.hasTestFiles = true
		}
		pa.collectDecls(f)
	}
	for _, f := range files {
		pa.collectImports(s, f)
	}
	for _, f := range files {
		pa.collectBodies(s, f)
	}
	return pa
}

func (s *seamScanner) importPath(dir string) string {
	rel, err := filepath.Rel(s.root, dir)
	if err != nil || rel == "." {
		return s.module
	}
	return s.module + "/" + filepath.ToSlash(rel)
}

// collectDecls records package-scope names, which is how the scanner knows an
// assignment target is a global rather than a local.
func (pa *pkgAnalysis) collectDecls(f *ast.File) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						// `var _ I = (*T)(nil)` declares nothing: the blank
						// identifier is never a variable, so `_ = x` is no write.
						if n.Name == "_" {
							continue
						}
						pa.pkgLevel[n.Name] = true
					}
				case *ast.TypeSpec:
					pa.pkgLevel[sp.Name.Name] = true
				}
			}
		case *ast.FuncDecl:
			// A method's name is scoped to its receiver type, not the package.
			if d.Recv == nil {
				pa.pkgLevel[d.Name.Name] = true
			}
		}
	}
}

func (pa *pkgAnalysis) collectImports(s *seamScanner, f *ast.File) {
	path := s.fset.Position(f.Pos()).Filename
	aliases := map[string]bool{}
	targets := map[string]string{}
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		alias := importPath
		if idx := strings.LastIndex(alias, "/"); idx >= 0 {
			alias = alias[idx+1:]
		}
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		aliases[alias] = strings.HasPrefix(importPath, s.module)
		targets[alias] = importPath
	}
	pa.imports[path] = aliases
	pa.crossTargets[path] = targets
}

func (pa *pkgAnalysis) collectBodies(s *seamScanner, f *ast.File) {
	path := s.fset.Position(f.Pos()).Filename
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		name := funcKey(fd)
		if _, seen := pa.funcs[name]; seen {
			continue
		}
		pa.funcs[name] = fd
		pa.fileOf[name] = path
		pa.lineOf[name] = s.fset.Position(fd.Pos()).Line
		if hasWaiver(fd) {
			pa.waived[name] = true
		}
		if w := pa.directWrite(s, path, fd); w != nil {
			pa.direct[name] = w
		}
		pa.bodyOf[name] = fd.Body
	}
}

// funcKey names a declaration so that a method and a free function never share
// a key. A free function is keyed by its bare name, which is what a call like
// `Poke()` or `pkg.Poke()` names. A method is keyed by receiver type and name
// ("poker.Poke"), which no such call can name: keyed by bare name, whichever of
// the two was declared first stood in for both, so a free function inherited a
// same-named method's write (a false alarm) or hid its own write behind a clean
// method (a missed swap).
func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return recvTypeName(fd.Recv.List[0].Type) + "." + fd.Name.Name
}

// recvTypeName strips a receiver's pointer and type parameters down to the
// type's name: *T, T[K], and *T[K, V] all name T.
func recvTypeName(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.Ident:
			return e.Name
		default:
			return "?"
		}
	}
}

// computeCrossSeams records, per function, the calls it makes into other
// packages that end in a global write. It runs after every package's own
// writes are known, because that is when a callee can be called a seam.
func (pa *pkgAnalysis) computeCrossSeams(s *seamScanner) {
	for name, body := range pa.bodyOf {
		pa.crossSeam[name] = pa.crossWrites(s, pa.fileOf[name], body)
	}
}

// candidateWrite is an assignment that targets process-global state. Whether it
// counts as a seam depends on the shape around it, which directWrite decides.
type candidateWrite struct {
	pos       token.Position
	what      string
	fromParam bool // the new value is a parameter the caller chose
	process   bool // os.Stdout and friends: global by definition
	reset     bool // a reset-named function zeroes, clears or deletes it
	swap      bool // a Swap, Store or CompareAndSwap call: a seam only when restored
}

// directWrite reports this function's own swap of process-global state, if it
// has one. It deliberately does not report every write to a package variable:
// a lazy cache or a counter is package state too, and flagging those buries the
// real seams in noise. A write counts when it has the seam's shape —
//
//   - the new value is a parameter, so the caller is choosing process state
//     (session.SetDefaultRegistry, slot.SetContainerListerForTest), or
//   - it is restored later in this same function (t.Cleanup, the shape that
//     makes a parallel caller lose the race), or
//   - a defer in this function writes the same global back (the swap is
//     scoped to the function rather than the test, but a parallel test that
//     does it races every parallel reader just the same: gt witness's
//     nudgeRefinery swap), or
//   - it is os.Stdout/os.Stderr/os.Stdin, which are process-global whatever
//     the value and cannot be made safe by construction, or
//   - it is a Swap, Store or CompareAndSwap on a package-level value with no
//     key argument (atomic.Value, atomic.Pointer), and a t.Cleanup or defer in
//     this function restores that same value. A sync.Map takes a key, so its
//     Store and Swap are not counted, and neither is its Delete. A bare Store
//     with no restore is a setter, not a swap, and is not a seam here, and
//   - the function is named as a reset (resetName) and zeroes, clears or
//     deletes a package variable. This is a seam with no parameter and no
//     restore: it empties process state that every parallel reader shares. A
//     delete keyed by a parameter counts here because of the name; the same
//     delete in a function not named a reset is a per-key write and is not.
//
// Locals are collected first so a shadowing `:=` inside the function is not
// mistaken for a write to the package-scope name it shadows.
func (pa *pkgAnalysis) directWrite(s *seamScanner, path string, fd *ast.FuncDecl) *globalWrite {
	locals := map[string]bool{}
	params := map[string]bool{}
	ast.Inspect(fd, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if node.Tok == token.DEFINE {
				for _, lhs := range node.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						locals[id.Name] = true
					}
				}
			}
		case *ast.DeclStmt:
			if gd, ok := node.Decl.(*ast.GenDecl); ok {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, id := range vs.Names {
							locals[id.Name] = true
						}
					}
				}
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{node.Key, node.Value} {
				if id, ok := e.(*ast.Ident); ok {
					locals[id.Name] = true
				}
			}
		case *ast.FuncLit:
			for _, fl := range []*ast.FieldList{node.Type.Params, node.Type.Results} {
				if fl == nil {
					continue
				}
				for _, field := range fl.List {
					for _, id := range field.Names {
						locals[id.Name] = true
					}
				}
			}
		}
		return true
	})
	if fd.Recv != nil {
		for _, field := range fd.Recv.List {
			for _, id := range field.Names {
				locals[id.Name] = true
			}
		}
	}
	for _, field := range fd.Type.Params.List {
		for _, id := range field.Names {
			locals[id.Name] = true
			params[id.Name] = true
		}
	}
	// Named results are locals too; writing one is no global write even when
	// a package-level func or method shares its name.
	if fd.Type.Results != nil {
		for _, field := range fd.Type.Results.List {
			for _, id := range field.Names {
				locals[id.Name] = true
			}
		}
	}

	var candidates []candidateWrite
	withCleanup := false
	deferRestored := map[string]bool{}   // globals a deferred call writes back
	cleanupRestored := map[string]bool{} // swapped globals a t.Cleanup stores back
	resetNamed := resetName(fd.Name.Name)
	once := onceRanges(fd, pa.pkgLevel)
	ast.Inspect(fd, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.DeferStmt:
			// `defer func() { X = old }()` restores X when this function
			// returns: the same swap-and-restore shape as t.Cleanup, scoped to
			// the function instead of the test. A parallel test that swaps
			// this way races every parallel reader of X for its whole body.
			// Only a defer that writes a global back counts; a defer that
			// unlocks a mutex says nothing about the writes around it. A
			// `defer X.Store(old)` restores an atomic value the same way.
			ast.Inspect(node.Call, func(m ast.Node) bool {
				if as, ok := m.(*ast.AssignStmt); ok && as.Tok != token.DEFINE {
					for _, lhs := range as.Lhs {
						if what, _, ok := pa.globalTarget(lhs, locals); ok {
							deferRestored[what] = true
						}
					}
				}
				if call, ok := m.(*ast.CallExpr); ok {
					if what, ok := pa.swapCall(call, locals); ok {
						deferRestored[what] = true
					}
				}
				return true
			})
		case *ast.AssignStmt:
			if node.Tok == token.DEFINE || within(node.Pos(), once) {
				return true
			}
			for i, lhs := range node.Lhs {
				what, process, ok := pa.globalTarget(lhs, locals)
				if !ok {
					continue
				}
				var rhs ast.Expr
				if i < len(node.Rhs) {
					rhs = node.Rhs[i]
				}
				candidates = append(candidates, candidateWrite{
					pos:       s.fset.Position(node.Pos()),
					what:      what,
					fromParam: rhs != nil && mentionsAny(rhs, params),
					process:   process,
					reset:     resetNamed && rhs != nil && isZeroValue(rhs),
				})
			}
		case *ast.CallExpr:
			if within(node.Pos(), once) {
				return true
			}
			if what, ok := processEnvMutation(node); ok {
				// Not marked process: production code sets env for a child
				// process ("if err := os.Setenv(...)") and is no seam at all.
				// Requiring a parameter value or a cleanup keeps this to the
				// helpers that hand process state to a caller.
				candidates = append(candidates, candidateWrite{
					pos:       s.fset.Position(node.Pos()),
					what:      what,
					fromParam: mentionsAny(node, params),
				})
			}
			if what, ok := pa.swapCall(node, locals); ok {
				candidates = append(candidates, candidateWrite{
					pos:  s.fset.Position(node.Pos()),
					what: what,
					swap: true,
				})
			}
			if what, ok := pa.resetCall(node, locals); ok && resetNamed {
				candidates = append(candidates, candidateWrite{
					pos:   s.fset.Position(node.Pos()),
					what:  what,
					reset: true,
				})
			}
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Cleanup" && len(node.Args) == 1 {
				withCleanup = true
				// A swap is restored when the cleanup stores the same value back.
				ast.Inspect(node.Args[0], func(m ast.Node) bool {
					if call, ok := m.(*ast.CallExpr); ok {
						if what, ok := pa.swapCall(call, locals); ok {
							cleanupRestored[what] = true
						}
					}
					return true
				})
			}
		}
		return true
	})
	if len(candidates) == 0 {
		return nil
	}
	for _, c := range candidates {
		if c.swap {
			if deferRestored[c.what] || cleanupRestored[c.what] {
				return &globalWrite{pos: c.pos, what: c.what}
			}
			continue
		}
		if c.process || c.fromParam || c.reset || deferRestored[c.what] {
			return &globalWrite{pos: c.pos, what: c.what, reset: c.reset}
		}
	}
	if withCleanup {
		// A swap nothing restores still hands process state to the caller, so
		// it is a seam on the same terms as a restored one. A method-form swap
		// is judged above, by its own restore, so it is not the fallback.
		for _, c := range candidates {
			if !c.swap {
				return &globalWrite{pos: c.pos, what: c.what}
			}
		}
	}
	return nil
}

// onceRange is a span of source the process executes at most once, because it
// is the body of a package-level sync.Once.
type onceRange struct {
	start, end token.Pos
}

// onceRanges finds the bodies of `onceVar.Do(func(){...})` where onceVar is a
// package-level variable. A swap in there is not a seam: sync.Once serializes
// the callers and nothing ever restores, so parallel tests cannot interleave.
// This is the shape gt-k317 asks new helpers to use, and without recognizing it
// the guard would flag the recommended fix.
func onceRanges(node ast.Node, pkgLevel map[string]bool) []onceRange {
	var out []onceRange
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Do" {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || !pkgLevel[recv.Name] {
			return true
		}
		lit, ok := call.Args[0].(*ast.FuncLit)
		if !ok {
			return true
		}
		out = append(out, onceRange{start: lit.Pos(), end: lit.End()})
		return true
	})
	return out
}

func within(pos token.Pos, ranges []onceRange) bool {
	for _, r := range ranges {
		if pos >= r.start && pos <= r.end {
			return true
		}
	}
	return false
}

// mentionsAny reports whether an expression reads any of the named identifiers.
func mentionsAny(expr ast.Expr, names map[string]bool) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && names[id.Name] {
			found = true
			return false
		}
		return !found
	})
	return found
}

// globalTarget names the global an assignment target writes, or reports false
// for locals and for state this guard does not model. The second result is
// true for the os streams, which are process-global whatever the value.
func (pa *pkgAnalysis) globalTarget(lhs ast.Expr, locals map[string]bool) (what string, process, ok bool) {
	switch target := lhs.(type) {
	case *ast.Ident:
		if locals[target.Name] || !pa.pkgLevel[target.Name] {
			return "", false, false
		}
		return "package variable " + target.Name, false, true
	case *ast.SelectorExpr:
		base, ok := target.X.(*ast.Ident)
		if !ok || base.Name != "os" {
			return "", false, false
		}
		switch target.Sel.Name {
		case "Stdout", "Stderr", "Stdin":
			return "os." + target.Sel.Name, true, true
		}
	}
	return "", false, false
}

// swapCall names a Swap, Store or CompareAndSwap on a package-level value with
// no key: atomic.Value and atomic.Pointer, the shape useTestContainerInitSlots
// swaps and restores. The argument count is what tells the keyed forms apart:
// sync.Map's Store and Swap take a key and a value, and its CompareAndSwap a key
// and two values, so only one argument (two for CompareAndSwap) is unkeyed.
func (pa *pkgAnalysis) swapCall(call *ast.CallExpr, locals map[string]bool) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	base, ok := sel.X.(*ast.Ident)
	if !ok || locals[base.Name] || !pa.pkgLevel[base.Name] {
		return "", false
	}
	switch sel.Sel.Name {
	case "Swap", "Store":
		if len(call.Args) != 1 {
			return "", false
		}
	case "CompareAndSwap":
		if len(call.Args) != 2 {
			return "", false
		}
	default:
		return "", false
	}
	return "package variable " + base.Name, true
}

// resetCall names clear(v) or delete(v, k) on a package-level map or slice: the
// builtins that empty process state. directWrite counts them only in a
// reset-named function.
func (pa *pkgAnalysis) resetCall(call *ast.CallExpr, locals map[string]bool) (string, bool) {
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || (fn.Name != "clear" && fn.Name != "delete") || len(call.Args) == 0 {
		return "", false
	}
	base, ok := call.Args[0].(*ast.Ident)
	if !ok || locals[base.Name] || !pa.pkgLevel[base.Name] {
		return "", false
	}
	return "package variable " + base.Name, true
}

// resetName reports whether a function is named as one that resets process
// state. Only such a function's zeroing, clearing or deleting of a package
// variable is a seam: elsewhere those writes are lazy fills and per-key
// bookkeeping, which the guard deliberately ignores.
func resetName(name string) bool {
	return strings.HasPrefix(name, "Reset") ||
		strings.HasPrefix(name, "Clear") ||
		strings.HasPrefix(name, "Invalidate") ||
		strings.HasSuffix(name, "AndReset")
}

// isZeroValue reports whether an expression is an empty value: nil, false, 0,
// "", an empty composite literal, or make with no size (or a zero size).
func isZeroValue(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "nil" || e.Name == "false"
	case *ast.BasicLit:
		return e.Value == "0" || e.Value == `""`
	case *ast.CompositeLit:
		return len(e.Elts) == 0
	case *ast.CallExpr:
		fn, ok := e.Fun.(*ast.Ident)
		if !ok || fn.Name != "make" || len(e.Args) == 0 {
			return false
		}
		for _, size := range e.Args[1:] {
			if lit, ok := size.(*ast.BasicLit); !ok || lit.Value != "0" {
				return false
			}
		}
		return true
	}
	return false
}

// processEnvMutation catches the process-wide calls that are writes without
// being assignments. testing.T.Setenv is deliberately absent: the testing
// package panics when it is called from a parallel test, so the runtime
// already enforces it.
func processEnvMutation(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	base, ok := sel.X.(*ast.Ident)
	if !ok || base.Name != "os" {
		return "", false
	}
	switch sel.Sel.Name {
	case "Setenv", "Unsetenv", "Chdir":
		return "os." + sel.Sel.Name, true
	}
	return "", false
}

// crossWrites reports global writes this function reaches through another
// package — session.SetDefaultRegistry and friends. The callee must be a
// non-test function of ours that itself writes a global, so a call to a pure
// reader costs nothing.
func (pa *pkgAnalysis) crossWrites(s *seamScanner, path string, body *ast.BlockStmt) []globalWrite {
	var out []globalWrite
	once := onceRanges(body, pa.pkgLevel)
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if within(call.Pos(), once) {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base, ok := sel.X.(*ast.Ident)
		if !ok || !pa.imports[path][base.Name] {
			return true
		}
		target := pa.crossTargets[path][base.Name]
		// pkg.Name names a free function, and mutator keys free functions by
		// bare name and methods by Recv.Name, so this lookup cannot land on a
		// method that happens to share the name.
		if !s.mutator[target][sel.Sel.Name] {
			return true
		}
		out = append(out, globalWrite{
			pos:  s.fset.Position(call.Pos()),
			what: strings.TrimPrefix(target, s.module+"/") + "." + sel.Sel.Name,
		})
		return true
	})
	return out
}

// mutates answers transitively: this function writes a global, or calls one
// that does. The answer comes from writers, so cycles in the call graph need
// no special handling.
func (pa *pkgAnalysis) mutates(name string) bool {
	return pa.writers()[name]
}

// writers is the set of this package's functions that reach a global write:
// a direct writer, a caller of another package's seam, or a caller of a member
// of the set. It is the least fixed point of that rule over the call graph,
// computed once per package, so each parallel test's reach is a lookup instead
// of a fresh walk of the callees (which made the guard too slow under -race).
func (pa *pkgAnalysis) writers() map[string]bool {
	if pa.writerSet != nil {
		return pa.writerSet
	}
	set := map[string]bool{}
	for name := range pa.direct {
		set[name] = true
	}
	for name := range pa.funcs {
		if len(pa.crossSeam[name]) > 0 {
			set[name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for name, fd := range pa.funcs {
			if set[name] || fd == nil || fd.Body == nil {
				continue
			}
			if pa.callsWriter(name, set) {
				set[name] = true
				changed = true
			}
		}
	}
	pa.writerSet = set
	return set
}

// callsWriter reports whether name's body calls a function in set, outside a
// package-level sync.Once body, which runs at most once and so cannot race.
func (pa *pkgAnalysis) callsWriter(name string, set map[string]bool) bool {
	body := pa.funcs[name].Body
	once := onceRanges(body, pa.pkgLevel)
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if within(call.Pos(), once) {
			return true
		}
		for _, callee := range pa.calleeNames(call, pa.fileOf[name]) {
			if set[callee] {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// calleeNames lists the same-package function keys a call could name. A bare
// `f()` names a free function, whose key is its bare name; it never names a
// method, whose key is Recv.Name.
//
// A method call `x.M(...)` has no receiver type without type information, so
// it names each method of this package that writes a package variable, is
// called M, and takes a compatible argument count (methodsNamed). Matching only that set keeps
// the call graph from connecting a test to every unrelated M in the package. A
// call on an import alias is another package's function, which crossWrites
// handles, so it names nothing here.
func (pa *pkgAnalysis) calleeNames(call *ast.CallExpr, file string) []string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return []string{fun.Name}
	case *ast.SelectorExpr:
		if base, ok := fun.X.(*ast.Ident); ok {
			if _, isImport := pa.imports[file][base.Name]; isImport {
				return nil
			}
		}
		return pa.methodsNamed(fun.Sel.Name, call)
	}
	return nil
}

// methodsNamed lists this package's methods named name that write a package
// variable, and whose parameter list the call's argument count fits. The count
// filter is sound for what it rules out: a call with the wrong number of
// arguments cannot be that method. It is not resolution: a same-named method of
// another type with a fitting count still matches, and a variable written only
// through a further call is not listed (the residual). Methods that write the
// environment are not listed, since that case is out of scope.
func (pa *pkgAnalysis) methodsNamed(name string, call *ast.CallExpr) []string {
	var out []string
	for key, w := range pa.direct {
		fd := pa.funcs[key]
		if fd == nil || fd.Recv == nil || !strings.HasSuffix(key, "."+name) || !isPackageVar(w.what) {
			continue
		}
		if fitsArgs(fd.Type, call) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// fitsArgs reports whether call could pass the arguments ft declares. A single
// call argument may spread a multi-value result, so it fits any count.
func fitsArgs(ft *ast.FuncType, call *ast.CallExpr) bool {
	if len(call.Args) == 1 {
		if _, spread := call.Args[0].(*ast.CallExpr); spread {
			return true
		}
	}
	n, variadic := 0, false
	for _, field := range ft.Params.List {
		n += max(len(field.Names), 1)
		if _, ok := field.Type.(*ast.Ellipsis); ok {
			variadic = true
		}
	}
	if variadic {
		return len(call.Args) >= n-1
	}
	return len(call.Args) == n
}

// parallelSwaps is the guard's verdict for one directory: every test in it
// that runs in parallel and can reach a global write.
func (pa *pkgAnalysis) parallelSwaps() []string {
	out := append([]string{}, pa.parseErrors...)
	for name, fd := range pa.funcs {
		if fd == nil || fd.Recv != nil || !strings.HasPrefix(name, "Test") || pa.waived[name] || fd.Body == nil {
			continue
		}
		if !callsParallel(fd.Body) || !pa.mutates(name) {
			continue
		}
		rel, err := filepath.Rel(pa.root, pa.fileOf[name])
		if err != nil {
			rel = pa.fileOf[name]
		}
		out = append(out, rel+":"+strconv.Itoa(pa.lineOf[name])+" "+name+" — "+pa.why(name))
	}
	return out
}

// unfollowedCalls names, for each t.Parallel test in this package that the guard
// reads as clean, the first method call in its chain that the guard cannot
// follow past. methodsNamed resolves a method call to direct writers only, so a
// method that writes a global through a further call ends the chain there, and
// without this line the test would read clean in silence.
func (pa *pkgAnalysis) unfollowedCalls() []string {
	reach := pa.unfollowReach()
	var out []string
	for name, fd := range pa.funcs {
		if fd == nil || fd.Recv != nil || !strings.HasPrefix(name, "Test") || pa.waived[name] || fd.Body == nil {
			continue
		}
		if !callsParallel(fd.Body) || pa.mutates(name) || !reach[name] {
			continue
		}
		site := pa.unfollowedSite(name, reach, map[string]bool{})
		rel, err := filepath.Rel(pa.root, pa.fileOf[name])
		if err != nil {
			rel = pa.fileOf[name]
		}
		out = append(out, rel+":"+strconv.Itoa(pa.lineOf[name])+" "+name+" — unfollowed: reaches method "+site+
			", which writes a process global only through a further call the guard does not follow")
	}
	return out
}

// unfollowedSite names the first unfollowable method call reached from fn, going
// only through same-package callees in reach. It returns the method and the
// call's position, or "" when none is reached.
func (pa *pkgAnalysis) unfollowedSite(fn string, reach, seen map[string]bool) string {
	if seen[fn] {
		return ""
	}
	seen[fn] = true
	fd := pa.funcs[fn]
	if fd == nil || fd.Body == nil {
		return ""
	}
	once := onceRanges(fd.Body, pa.pkgLevel)
	var calls []*ast.CallExpr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && !within(call.Pos(), once) {
			calls = append(calls, call)
		}
		return true
	})
	for _, call := range calls {
		if key := pa.unfollowedMethod(call, pa.fileOf[fn]); key != "" {
			return key + " at " + pa.fset.Position(call.Pos()).String()
		}
	}
	for _, call := range calls {
		for _, callee := range pa.calleeNames(call, pa.fileOf[fn]) {
			if !reach[callee] {
				continue
			}
			if site := pa.unfollowedSite(callee, reach, seen); site != "" {
				return site
			}
		}
	}
	return ""
}

// unfollowedMethod names the method a call could be when that method writes a
// package variable only through a further call, and no direct writer matches
// the call. Such a method is the one the guard cannot follow past. It returns ""
// for every other call, including calls into another package.
func (pa *pkgAnalysis) unfollowedMethod(call *ast.CallExpr, file string) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if base, ok := sel.X.(*ast.Ident); ok {
		if _, isImport := pa.imports[file][base.Name]; isImport {
			return ""
		}
	}
	if len(pa.methodsNamed(sel.Sel.Name, call)) > 0 {
		return ""
	}
	for _, key := range pa.indirectMethods()[sel.Sel.Name] {
		if fitsArgs(pa.funcs[key].Type, call) {
			return key
		}
	}
	return ""
}

// indirectMethods indexes, by method name, this package's methods that reach a
// global write without being a direct writer: the ones the guard cannot follow
// past. Each list is sorted, so the named method is the same on every run.
func (pa *pkgAnalysis) indirectMethods() map[string][]string {
	if pa.indirect != nil {
		return pa.indirect
	}
	idx := map[string][]string{}
	for key := range pa.writers() {
		fd := pa.funcs[key]
		if fd == nil || fd.Recv == nil || pa.direct[key] != nil {
			continue
		}
		name := key[strings.LastIndex(key, ".")+1:]
		idx[name] = append(idx[name], key)
	}
	for _, keys := range idx {
		sort.Strings(keys)
	}
	pa.indirect = idx
	return idx
}

// unfollowReach is the set of this package's functions whose same-package chain
// reaches an unfollowable method call. A function is in it when it makes one
// directly, or calls a member, the same least fixed point writers() computes.
// It is memoized, so each parallel test's chain is a lookup, not a fresh walk.
func (pa *pkgAnalysis) unfollowReach() map[string]bool {
	if pa.unfollowSet != nil {
		return pa.unfollowSet
	}
	set := map[string]bool{}
	for name, fd := range pa.funcs {
		if fd == nil || fd.Body == nil {
			continue
		}
		if pa.hasUnfollowedCall(name) {
			set[name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for name, fd := range pa.funcs {
			if set[name] || fd == nil || fd.Body == nil {
				continue
			}
			if pa.callsWriter(name, set) {
				set[name] = true
				changed = true
			}
		}
	}
	pa.unfollowSet = set
	return set
}

// hasUnfollowedCall reports whether name's own body makes an unfollowable method
// call, outside a package-level sync.Once body.
func (pa *pkgAnalysis) hasUnfollowedCall(name string) bool {
	body := pa.funcs[name].Body
	once := onceRanges(body, pa.pkgLevel)
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || within(call.Pos(), once) {
			return true
		}
		if pa.unfollowedMethod(call, pa.fileOf[name]) != "" {
			found = true
			return false
		}
		return true
	})
	return found
}

func callsParallel(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "Parallel" && len(call.Args) == 0 {
			found = true
			return false
		}
		return true
	})
	return found
}

// why renders the shortest chain from a flagged test to a global write, so the
// failure message says which helper to fix instead of only naming the test.
func (pa *pkgAnalysis) why(name string) string {
	chain := []string{}
	seen := map[string]bool{}
	for cur := name; cur != "" && !seen[cur]; {
		seen[cur] = true
		if w, ok := pa.direct[cur]; ok {
			chain = append(chain, cur+" writes "+w.what+" at "+w.pos.String())
			return strings.Join(chain, " -> ")
		}
		if ws := pa.crossSeam[cur]; len(ws) > 0 {
			chain = append(chain, cur+" calls "+ws[0].what+" at "+ws[0].pos.String())
			return strings.Join(chain, " -> ")
		}
		next := pa.nextMutator(cur, seen)
		if next == "" {
			break
		}
		chain = append(chain, cur)
		cur = next
	}
	if len(chain) == 0 {
		return "reaches a process-global write"
	}
	return strings.Join(chain, " -> ") + " -> a process-global write"
}

// nextMutator picks the first same-package callee that mutates. Determinism
// matters only for readable output, so declaration order is enough.
func (pa *pkgAnalysis) nextMutator(name string, seen map[string]bool) string {
	fd := pa.funcs[name]
	if fd == nil || fd.Body == nil {
		return ""
	}
	var out string
	once := onceRanges(fd.Body, pa.pkgLevel)
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if out != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if within(call.Pos(), once) {
			return true
		}
		for _, callee := range pa.calleeNames(call, pa.fileOf[name]) {
			if seen[callee] {
				continue
			}
			if pa.mutates(callee) {
				out = callee
				return false
			}
		}
		return true
	})
	return out
}

// hasWaiver reports whether the test carries the opt-out in its own text: its
// doc comment, or a comment anywhere in its body.
func hasWaiver(fd *ast.FuncDecl) bool {
	groups := []*ast.CommentGroup{}
	if fd.Doc != nil {
		groups = append(groups, fd.Doc)
	}
	if fd.Body != nil {
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if cg, ok := n.(*ast.CommentGroup); ok {
				groups = append(groups, cg)
			}
			return true
		})
	}
	for _, cg := range groups {
		for _, c := range cg.List {
			if strings.Contains(c.Text, parallelGlobalWaiver) {
				return true
			}
		}
	}
	return false
}
