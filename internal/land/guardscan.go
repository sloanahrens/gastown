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

// bodyListsFiles reports whether any string literal in body, including inside a
// function literal, contains "ls-files". A command held in a variable or built
// by concatenation is not seen, and a nil body is false.
func bodyListsFiles(body *ast.BlockStmt) bool {
	if body == nil {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		found = found || ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "ls-files")
		return !found
	})
	return found
}

// scanFile returns, in source order, the names of the top-level test functions
// in src that walk the tree from a parent or hold a string literal containing
// "ls-files", directly or through a plain-named same-file function, each
// function once. Names resolve by spelling: a selector call, an undefined name
// or another file's helper is not followed.
func scanFile(name string, src []byte) ([]string, error) {
	f, err := parseGoFile(name, src)
	if err != nil {
		return nil, err
	}
	same := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil {
			same[fn.Name.Name] = fn
		}
	}
	var names []string
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && isRunTest(fn) && scansTree(fn, same) {
			names = append(names, fn.Name.Name)
		}
	}
	return names, nil
}

// scansTree reports whether fn's body, or a plain-named function in same that fn
// reaches transitively, holds what bodyWalksParent or bodyListsFiles matches. A
// function is visited once, so a recursive helper pair ends.
func scansTree(fn *ast.FuncDecl, same map[string]*ast.FuncDecl) bool {
	seen := map[string]bool{}
	for todo := []*ast.FuncDecl{fn}; len(todo) > 0; todo = todo[1:] {
		f := todo[0]
		if f == nil || f.Body == nil || seen[f.Name.Name] {
			continue
		}
		seen[f.Name.Name] = true
		if bodyWalksParent(f.Body) || bodyListsFiles(f.Body) {
			return true
		}
		ast.Inspect(f.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					todo = append(todo, same[id.Name])
				}
			}
			return true
		})
	}
	return false
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

// ScanningTest names one test function that scans the source tree across a
// directory: Pkg is its slash-separated package directory, Name the function.
type ScanningTest struct{ Pkg, Name string }

// scanSkipDirs are the directory names ScanningTests never descends into.
var scanSkipDirs = map[string]bool{"testdata": true, "vendor": true, "node_modules": true}

// ScanningTests returns, sorted by package then name, the tests in fsys that
// walk the tree from a parent or hold a string literal containing "ls-files",
// directly or through a same-file helper. It reads every _test.go file except
// under testdata, vendor, node_modules and dot directories, and returns the
// first error from reading a file or from scanFile.
func ScanningTests(fsys fs.FS) ([]ScanningTest, error) {
	var found []ScanningTest
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != "." && (scanSkipDirs[name] || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		names, err := scanFile(p, src)
		if err != nil {
			return err
		}
		for _, n := range names {
			found = append(found, ScanningTest{Pkg: path.Dir(p), Name: n})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Pkg != found[j].Pkg {
			return found[i].Pkg < found[j].Pkg
		}
		return found[i].Name < found[j].Name
	})
	return found, nil
}
