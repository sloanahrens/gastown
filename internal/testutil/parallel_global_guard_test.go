package testutil

import (
	"go/ast"
	"go/parser"
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
func TestNoParallelTestsReachProcessGlobalSwaps(t *testing.T) {
	root := repoRoot(t)
	scanner := newSeamScanner(t, root)

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
	fixture := writeGuardFixture(t)
	scanner := newSeamScanner(t, fixture)
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
	}
	seen := map[string]bool{}
	for _, line := range got {
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
	for name, expect := range want {
		if expect && !seen[name] {
			t.Errorf("scanner missed %s; the fixture expects it reported.\nScanned %d violation(s):\n%s",
				name, len(got), strings.Join(got, "\n"))
		}
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
`,
		"pkg/seam_test.go": `package pkg

import (
	"os"
	"sync"
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
	root    string
	module  string
	fset    *token.FileSet
	dirs    []string
	pkgs    map[string]*pkgAnalysis // dir -> analysis
	mutator map[string]map[string]bool
}

func newSeamScanner(t *testing.T, root string) *seamScanner {
	t.Helper()
	return &seamScanner{
		root:    root,
		module:  modulePath(t, root),
		fset:    token.NewFileSet(),
		pkgs:    map[string]*pkgAnalysis{},
		mutator: map[string]map[string]bool{},
	}
}

func modulePath(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("go.mod at %s has no module line", root)
	return ""
}

func (s *seamScanner) runDirs(t *testing.T) []string {
	t.Helper()
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		switch d.Name() {
		case "vendor", ".git", "testdata", "node_modules":
			return filepath.SkipDir
		}
		// A nested module (plugins/dolt-snapshots) has its own import paths;
		// resolving its packages against this module's prefix would be wrong.
		if path != s.root {
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
				return filepath.SkipDir
			}
		}
		s.dirs = append(s.dirs, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", s.root, err)
	}
	sort.Strings(s.dirs)
	return s.dirs
}

// scan runs in three passes because the passes depend on each other in one
// direction. Parsing a directory cannot resolve a call into another package
// until every package's own global writes are known, so the cross-package
// edges are a second pass over the already-parsed trees rather than part of
// parsing them.
func (s *seamScanner) scan(t *testing.T) []string {
	t.Helper()
	s.runDirs(t)
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
		mut := map[string]bool{}
		for name := range pa.direct {
			mut[name] = true
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
		if pa == nil || !pa.hasTestFiles {
			continue
		}
		violations = append(violations, pa.parallelSwaps()...)
	}
	sort.Strings(violations)
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
	pos  token.Position
	what string
}

// pkgAnalysis holds one directory's parsed package. Test and non-test files are
// merged: a parallel test reaches both its own package's helpers and the
// package's production code, and either can write a global.
type pkgAnalysis struct {
	root         string // scan root, so reported paths are relative to it
	importPath   string
	pkgLevel     map[string]bool              // package-scope names declared in this dir
	funcs        map[string]*ast.FuncDecl     // func name -> declaration
	bodyOf       map[string]*ast.BlockStmt    // func name -> body
	lineOf       map[string]int               // func name -> declaration line
	fileOf       map[string]string            // func name -> source path
	waived       map[string]bool              // func name -> carries the waiver comment
	direct       map[string]*globalWrite      // func name -> its own global write
	crossSeam    map[string][]globalWrite     // func name -> global writes via other packages
	imports      map[string]map[string]bool   // file path -> alias -> is-our-module
	crossTargets map[string]map[string]string // file path -> alias -> import path
	parseErrors  []string
	hasTestFiles bool
}

func parseDir(s *seamScanner, dir string) *pkgAnalysis {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	pa := &pkgAnalysis{
		root:         s.root,
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
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(s.fset, path, nil, parser.ParseComments)
		if err != nil {
			// A file this package cannot parse is this guard's problem to
			// report, not to skip: silently ignoring it would let a swap hide
			// behind a syntax error.
			pa.parseErrors = append(pa.parseErrors, path+": "+err.Error())
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
						pa.pkgLevel[n.Name] = true
					}
				case *ast.TypeSpec:
					pa.pkgLevel[sp.Name.Name] = true
				}
			}
		case *ast.FuncDecl:
			pa.pkgLevel[d.Name.Name] = true
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
		name := fd.Name.Name
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
//   - it is os.Stdout/os.Stderr/os.Stdin, which are process-global whatever
//     the value and cannot be made safe by construction.
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
			if node.Type.Params != nil {
				for _, field := range node.Type.Params.List {
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

	var candidates []candidateWrite
	withCleanup := false
	once := onceRanges(fd, pa.pkgLevel)
	ast.Inspect(fd, func(n ast.Node) bool {
		switch node := n.(type) {
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
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Cleanup" && len(node.Args) == 1 {
				withCleanup = true
			}
		}
		return true
	})
	if len(candidates) == 0 {
		return nil
	}
	for _, c := range candidates {
		if c.process || c.fromParam {
			return &globalWrite{pos: c.pos, what: c.what}
		}
	}
	if withCleanup {
		// A swap nothing restores still hands process state to the caller, so
		// it is a seam on the same terms as a restored one.
		return &globalWrite{pos: candidates[0].pos, what: candidates[0].what}
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
// that does. The stack breaks cycles, which the call graph can have.
func (pa *pkgAnalysis) mutates(name string, stack map[string]bool) bool {
	if stack[name] {
		return false
	}
	if _, ok := pa.direct[name]; ok {
		return true
	}
	if len(pa.crossSeam[name]) > 0 {
		return true
	}
	fd, ok := pa.funcs[name]
	if !ok || fd == nil || fd.Body == nil {
		return false
	}
	stack[name] = true
	defer delete(stack, name)
	once := onceRanges(fd.Body, pa.pkgLevel)
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
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
		for _, callee := range calleeNames(call, pa.imports[pa.fileOf[name]]) {
			if pa.mutates(callee, stack) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// calleeNames lists the same-package function names a call could name.
//
// Method calls are deliberately not followed. Without type information a
// `x.Run()` can only be matched by the name "Run", and every package in this
// repo has several unrelated ones — following them connects a test to half the
// package and buries the real seams. Test helpers are free functions in
// practice, and the cost of missing a method-shaped one is a missed finding,
// not a wrong one. Calls into other packages are crossWrites' job.
func calleeNames(call *ast.CallExpr, ourAliases map[string]bool) []string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return []string{fun.Name}
	case *ast.SelectorExpr:
		if base, ok := fun.X.(*ast.Ident); ok && ourAliases[base.Name] {
			return nil
		}
	}
	return nil
}

// parallelSwaps is the guard's verdict for one directory: every test in it
// that runs in parallel and can reach a global write.
func (pa *pkgAnalysis) parallelSwaps() []string {
	out := append([]string{}, pa.parseErrors...)
	for name, fd := range pa.funcs {
		if !strings.HasPrefix(name, "Test") || pa.waived[name] || fd == nil || fd.Body == nil {
			continue
		}
		if !callsParallel(fd.Body) || !pa.mutates(name, map[string]bool{}) {
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
		for _, callee := range calleeNames(call, pa.imports[pa.fileOf[name]]) {
			if seen[callee] {
				continue
			}
			if pa.mutates(callee, map[string]bool{}) {
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
