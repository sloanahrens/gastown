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

// TestDashboardHubWiresTheDeepSeekRowsReadToResolveSpend guards the spend
// source's production wiring at its call site: newDashboardHub hands resolveSpend
// the dashboard beads handle's costSamples read, so the COST pane reads the
// town's deepseek_balance rows before any file (gt-u3fp8i). The hub's spend
// closure is not reachable from a unit test without the real environment, which
// the unit tier forbids, so the argument is read from the function body, as
// TestDashboardHubReadsTitlesThroughOpenDashboardBeads reads the reader choice.
func TestDashboardHubWiresTheDeepSeekRowsReadToResolveSpend(t *testing.T) {
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
	var calls []*ast.CallExpr
	ast.Inspect(hub.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "resolveSpend" {
			calls = append(calls, call)
		}
		return true
	})
	if len(calls) != 1 {
		t.Fatalf("newDashboardHub calls resolveSpend %d times, want 1", len(calls))
	}
	if len(calls[0].Args) != 3 {
		t.Fatalf("resolveSpend is called with %d arguments, want the flag, the rows read and the log", len(calls[0].Args))
	}
	sel, ok := calls[0].Args[1].(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "costSamples" {
		t.Fatalf("resolveSpend's rows argument is %T, want beadReads.costSamples", calls[0].Args[1])
	}
	if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "beadReads" {
		t.Errorf("resolveSpend's rows come from %T, want the dashboard beads handle beadReads", sel.X)
	}
}
