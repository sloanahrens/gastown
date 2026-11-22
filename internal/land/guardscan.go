package land

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
// os.ReadDir or fs.WalkDir whose root argument holds a string literal starting
// "..". A root reached through a variable or a helper is not seen.
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
	return strings.HasPrefix(lit.Value[1:], "..")
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
