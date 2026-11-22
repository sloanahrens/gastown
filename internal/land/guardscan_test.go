package land

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestCallWalksParent(t *testing.T) {
	t.Parallel()
	for expr, want := range map[string]bool{
		`filepath.WalkDir("../..", f)`: true, `filepath.Walk("..", f)`: true, `os.ReadDir("../sibling")`: true,
		`fs.WalkDir(fsys, "../..", f)`: true, `fs.WalkDir("../..", fsys, f)`: false,
		`filepath.WalkDir(t.TempDir(), f)`: false, `filepath.WalkDir("internal", f)`: false,
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
