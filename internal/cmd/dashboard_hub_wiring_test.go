package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDashboardHubReadsTitlesThroughOpenDashboardBeads guards the hub's reader
// choice at its call site: newDashboardHub builds its title reader through
// openDashboardBeads, which carries dashboardTitleCacheSize, and through no
// other reader constructor. The hub returns a dashboard.Hub that does not
// expose the reader, and the test policy forbids swapping a package seam, so
// the choice is read from the function body itself. A hub that calls
// openTailBeads or newTailBeads directly, with any bound, fails here; the
// bound itself is TestDashboardTitleCacheHoldsAFullPage's (gt-bjhc3r).
func TestDashboardHubReadsTitlesThroughOpenDashboardBeads(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "dashboard.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing dashboard.go: %v", err)
	}
	var hub *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "newDashboardHub" {
			hub = fn
		}
	}
	if hub == nil {
		t.Fatal("dashboard.go has no newDashboardHub")
	}
	calls := map[string]int{}
	ast.Inspect(hub.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			calls[id.Name]++
		}
		return true
	})
	if calls["openDashboardBeads"] != 1 {
		t.Errorf("newDashboardHub calls openDashboardBeads %d times, want 1: the hub's titles come from the dashboard-bounded reader", calls["openDashboardBeads"])
	}
	for _, other := range []string{"openTailBeads", "newTailBeads", "newTailBeadsFromStore"} {
		if calls[other] != 0 {
			t.Errorf("newDashboardHub calls %s %d times, want 0: a reader built there carries a bound the dashboard did not choose", other, calls[other])
		}
	}
}
