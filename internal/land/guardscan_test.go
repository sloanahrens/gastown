package land

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestCallWalksParent(t *testing.T) {
	t.Parallel()
	for expr, want := range map[string]bool{
		`filepath.WalkDir("../..", f)`: true, `filepath.Walk("..", f)`: true, `os.ReadDir("../sibling")`: true,
		`fs.WalkDir(fsys, "../..", f)`: true, `fs.WalkDir("../..", fsys, f)`: false,
		`filepath.WalkDir(t.TempDir(), f)`: false, `filepath.WalkDir("internal", f)`: false,
		// ".." is a parent only as a whole path element.
		`filepath.WalkDir("..", f)`: true, `filepath.WalkDir("../x", f)`: true,
		`filepath.WalkDir("..foo", f)`: false, `filepath.WalkDir(".", f)`: false,
		`filepath.WalkDir(".hidden", f)`: false,
		// A raw-string literal is read like an interpreted one.
		"filepath.WalkDir(`../x`, f)": true,
		// A call with fewer arguments than the walker takes is false, not a panic.
		`filepath.WalkDir()`: false, `os.ReadDir()`: false, `fs.WalkDir(fsys)`: false,
		// fp aliases path/filepath, but callWalksParent reads the package name
		// as spelled and does not resolve imports: an aliased walk is not seen.
		`fp.WalkDir("..", f)`: false,
	} {
		call, err := parser.ParseExpr(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		if got := callWalksParent(call.(*ast.CallExpr)); got != want {
			t.Errorf("callWalksParent(%s) = %v, want %v", expr, got, want)
		}
	}
}

func TestBodyWalksParent(t *testing.T) {
	t.Parallel()
	for decl, want := range map[string]bool{
		`func F()`: false, // no body
		`func F() { filepath.WalkDir("../..", f) }`:        true,
		`func F() { defer func() { os.ReadDir("..") }() }`: true,
		`func F() { filepath.WalkDir(t.TempDir(), f) }`:    false,
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "", "package p\n"+decl, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", decl, err)
		}
		if got := bodyWalksParent(f.Decls[0].(*ast.FuncDecl).Body); got != want {
			t.Errorf("bodyWalksParent(%s) = %v, want %v", decl, got, want)
		}
	}
}

func TestParseGoFile(t *testing.T) {
	t.Parallel()
	for src, wantErr := range map[string]bool{
		"package p\n\nfunc F() {}": false,
		"package p\n\nfunc (":      true,
	} {
		f, err := parseGoFile("file.go", []byte(src))
		if !wantErr && (f == nil || err != nil) {
			t.Fatalf("parseGoFile(%q) = %v, %v; want a parsed file", src, f, err)
		}
		if wantErr && (err == nil || !strings.HasPrefix(err.Error(), "file.go") || errors.Unwrap(err) == nil) {
			t.Errorf("parseGoFile(%q) error = %v, want the name first and the parser error wrapped", src, err)
		}
	}
}

func TestIsRunTest(t *testing.T) {
	t.Parallel()
	for decl, want := range map[string]bool{
		"func Test(t *testing.T) {}": true, "func TestA() {}": true, "func Test1() {}": true,
		"func Test_x() {}":    true, // go test runs Test_x: '_' is not lower case
		"func Testimony() {}": false, "func testA() {}": false, "func (s S) TestA() {}": false,
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "", "package p\n"+decl, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", decl, err)
		}
		if got := isRunTest(f.Decls[0].(*ast.FuncDecl)); got != want {
			t.Errorf("isRunTest(%s) = %v, want %v", decl, got, want)
		}
	}
}
