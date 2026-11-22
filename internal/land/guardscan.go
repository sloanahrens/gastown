package land

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// walkerRoots gives each tree walk's root argument index; fs.WalkDir takes the
// file system first.
var walkerRoots = map[string]int{
	"filepath.Walk": 0, "filepath.WalkDir": 0, "os.ReadDir": 0, "fs.WalkDir": 1,
}

// callWalksParent reports whether call is a filepath.Walk, filepath.WalkDir,
// os.ReadDir or fs.WalkDir whose root argument holds a string literal that is
// ".." or begins "../". A root reached through a variable, a helper or an
// aliased import is not seen.
func callWalksParent(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	root, ok := walkerRoots[pkg.Name+"."+sel.Sel.Name]
	if !ok || root >= len(call.Args) {
		return false
	}
	lit, ok := call.Args[root].(*ast.BasicLit)
	if !ok || len(lit.Value) < 2 || (lit.Value[0] != '"' && lit.Value[0] != '`') {
		return false
	}
	text := lit.Value[1:]
	if !strings.HasPrefix(text, "..") {
		return false
	}
	// ".." must be a whole path element: the literal ends or a separator
	// follows. "..foo" names a sibling, not the parent.
	rest := text[2:]
	return rest == lit.Value[:1] || strings.HasPrefix(rest, "/")
}

// bodyWalksParent reports whether any call in body, at any depth including
// inside a function literal, walks the tree from a parent. A nil body is false.
func bodyWalksParent(body *ast.BlockStmt) bool {
	if body == nil {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		found = found || ok && callWalksParent(call)
		return !found
	})
	return found
}

// scanFile returns, in source order, the names of the top-level test functions
// in src whose own body walks the tree from a parent. A root reached through a
// helper is not seen here: a helper in the same file is followed by a later
// bead, and a helper in another file is never seen.
func scanFile(name string, src []byte) ([]string, error) {
	f, err := parseGoFile(name, src)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && isRunTest(fn) && bodyWalksParent(fn.Body) {
			names = append(names, fn.Name.Name)
		}
	}
	return names, nil
}

func parseGoFile(name string, src []byte) (*ast.File, error) {
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return f, nil
}

// isRunTest reports whether go test runs fn as a test: a top-level function
// whose name is "Test", or "Test" followed by a rune unicode.IsLower reports
// false for, as in cmd/go's isTest. A method is not a test.
func isRunTest(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
		return false
	}
	rest := fn.Name.Name[len("Test"):]
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

type ScanningTest struct{ Pkg, Name string }

// ScanningTests returns the tests in fsys whose own body walks a parent, sorted by
// package then name, skipping testdata/vendor/node_modules/dot dirs and returning any error.
func ScanningTests(fsys fs.FS) (g []ScanningTest, err error) {
	if err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if n := d.Name(); d.IsDir() && p != "." && (n == "testdata" || n == "vendor" || n == "node_modules" || n[0] == '.') {
			return fs.SkipDir
		} else if d.IsDir() || !strings.HasSuffix(n, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err == nil {
			var names []string
			names, err = scanFile(p, src)
			for _, name := range names {
				g = append(g, ScanningTest{path.Dir(p), name})
			}
		}
		return err
	}); err != nil {
		return nil, err
	}
	sort.Slice(g, func(i, j int) bool { return g[i].Pkg < g[j].Pkg || g[i].Pkg == g[j].Pkg && g[i].Name < g[j].Name })
	return g, nil
}
