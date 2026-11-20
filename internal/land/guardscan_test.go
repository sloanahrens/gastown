package land

import (
	"go/ast"
	"go/parser"
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
