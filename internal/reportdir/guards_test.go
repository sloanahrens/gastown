package reportdir

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// reportdirNames resolves the names this package exports when another file
// writes reportdir.<Name>, so the guard reads that call the same as the string.
var reportdirNames = map[string]string{
	"SidecarName":      SidecarName,
	"PageName":         PageName,
	"PrevPageName":     PrevPageName,
	"PrevSidecarName":  PrevSidecarName,
	"PublishedDirName": PublishedDirName,
	"ReportsSubdir":    ReportsSubdir,
}

// TestPathGuardFailsOnAFixtureOutsideThePackage is the reports-path guard: no
// non-test file outside this package may lay a report's path out by hand. The
// repository is clean, and a fixture outside the package that joins the reports
// directory with a daily file makes the same scan fail and names the file.
func TestPathGuardFailsOnAFixtureOutsideThePackage(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	ownPackage := filepath.Join("internal", "reportdir") + string(filepath.Separator)
	problems, err := reportPathProblems(root, ownPackage)
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("these join the reports directory with a daily file; read the directory through internal/reportdir: %v", problems)
	}

	fixture := filepath.Join(root, "testdata", "reportdir_guard")
	problems, err = reportPathProblems(fixture, "")
	if err != nil {
		t.Fatalf("walking the fixture: %v", err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "joins.go") {
		t.Fatalf("the guard did not fail on a file that builds the path: %v", problems)
	}
}

// reportPathProblems returns one entry per function, in a non-test file under
// root, that both joins the reports directory name and joins a daily file
// name. Files whose path starts with skipPrefix are not read, so a package can
// exempt itself from its own guard.
func reportPathProblems(root, skipPrefix string) ([]string, error) {
	var problems []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "scratch" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if skipPrefix != "" && strings.HasPrefix(rel, skipPrefix) {
			return nil
		}
		problems = append(problems, functionJoinsReports(path, rel)...)
		return nil
	})
	return problems, err
}

// functionJoinsReports flags a function in one file that both joins the reports
// directory name and joins a daily file name.
func functionJoinsReports(path, rel string) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return []string{rel + ": " + err.Error()}
	}
	consts := stringConsts(file)
	var problems []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		var joinsReports, joinsDaily bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isJoinCall(call.Fun) {
				return true
			}
			for _, arg := range call.Args {
				switch literalString(arg, consts) {
				case PublishedDirName: // the same name as ReportsSubdir
					joinsReports = true
				case SidecarName, PageName, PrevPageName, PrevSidecarName:
					joinsDaily = true
				}
			}
			return true
		})
		if joinsReports && joinsDaily {
			problems = append(problems, rel+": "+fn.Name.Name)
		}
		return true
	})
	return problems
}

// isJoinCall reports whether fun is the path package's Join or filepath's.
func isJoinCall(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Join" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && (pkg.Name == "path" || pkg.Name == "filepath")
}

// literalString resolves an expression to the string it names: a literal, a
// constant declared in the same file, or one of this package's exported names.
// Anything else is the empty string, which matches no report name.
func literalString(expr ast.Expr, consts map[string]string) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return ""
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return ""
		}
		return s
	case *ast.Ident:
		return consts[e.Name]
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok && pkg.Name == "reportdir" {
			return reportdirNames[e.Sel.Name]
		}
	}
	return ""
}

// stringConsts is a file's single-name string constants, so a join written
// with a named file reads the same as the literal.
func stringConsts(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(lit.Value)
			if err == nil {
				out[vs.Names[0].Name] = s
			}
		}
	}
	return out
}

// TestReportdirImports is this package's import guard: internal/reportdir is a
// leaf, so its dependency set holds neither the bird pool nor the report
// reader. A fixture package under testdata that imports the bird pool makes
// the same walk see it, so the check can fail.
func TestReportdirImports(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	module := modulePath(t, root)
	self := module + "/internal/reportdir"
	deps := moduleDeps(t, root, module, self)
	if !deps[self] {
		t.Fatal("the dependency walk did not read internal/reportdir; it has stopped seeing the module")
	}
	for _, forbidden := range []string{module + "/internal/bird", module + "/internal/birdreports"} {
		if deps[forbidden] {
			t.Errorf("internal/reportdir depends on %s; the resolver must stay a leaf", forbidden)
		}
	}

	fixtureDeps := moduleDeps(t, root, module, module+"/testdata/reportdir_imports")
	if !fixtureDeps[module+"/internal/bird"] {
		t.Fatal("the walk did not follow the fixture's import of internal/bird; a fixture importing it could not fail this test")
	}
}

// modulePath is the module's import prefix, read from go.mod so a rename moves
// the walk with it.
func modulePath(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("go.mod names no module")
	return ""
}

// moduleDeps returns the module packages reachable from start, this module
// written as a go list -deps walk would: package directories, non-test files,
// and the module imports they name.
func moduleDeps(t *testing.T, root, module string, start ...string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	var walk func(importPath string)
	walk = func(importPath string) {
		if seen[importPath] {
			return
		}
		seen[importPath] = true
		dir, ok := moduleDir(root, module, importPath)
		if !ok {
			t.Fatalf("%s is not a path in this module", importPath)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		fset := token.NewFileSet()
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			af, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parsing %s: %v", filepath.Join(dir, name), err)
			}
			for _, imp := range af.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				if p == module || strings.HasPrefix(p, module+"/") {
					walk(p)
				}
			}
		}
	}
	for _, s := range start {
		walk(s)
	}
	return seen
}

// moduleDir maps a module import path to its directory.
func moduleDir(root, module, importPath string) (string, bool) {
	if importPath == module {
		return root, true
	}
	rel, ok := strings.CutPrefix(importPath, module+"/")
	if !ok {
		return "", false
	}
	return filepath.Join(root, filepath.FromSlash(rel)), true
}
