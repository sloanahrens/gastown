package polecat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoWorkstateInputLiteralsOutsideConstructor is the gt-hsg structural
// regression test. gt-7kr fixed a fail-open promotion in
// internal/polecat/manager.go's workstateInputForPolecat. gt-14a patched a
// second, independent copy of the same promotion in the CLI check-recovery
// handler (internal/cmd/polecat.go) — and missed a third: a different
// ungated bypass in the very same function, reachable via a different
// precondition (partial-spawn), that neither prior fix touched because
// nothing enforced that WorkstateInput could only be built one way.
//
// "Patch copy #3" would leave the class alive for a fourth generation. This
// test enforces the structural fix instead: every production package that
// builds a WorkstateInput must do so through polecat.NewWorkstateInput (the
// single constructor, in workstate.go), never through a WorkstateInput{}
// composite literal of its own. Test files are exempt — hand-constructing a
// WorkstateInput to exercise DecideWorkstate directly is how the decision
// layer gets tested, and workstate.go itself (home of the constructor) is
// exempt from the "elsewhere" check trivially.
func TestNoWorkstateInputLiteralsOutsideConstructor(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	packages := []string{
		"internal/polecat",
		"internal/cmd",
		"internal/witness",
	}

	var violations []string
	for _, pkg := range packages {
		dir := filepath.Join(repoRoot, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			if pkg == "internal/polecat" && name == "workstate.go" {
				continue // home of the constructor itself
			}
			path := filepath.Join(dir, name)
			violations = append(violations, workstateInputLiterals(t, repoRoot, path)...)
		}
	}

	if len(violations) > 0 {
		t.Fatalf("do not build WorkstateInput{} directly outside polecat.NewWorkstateInput; gather facts into a WorkstateFacts and call polecat.NewWorkstateInput(facts) instead, so the fail-closed CleanupStatus policy lives in exactly one place:\n%s", strings.Join(violations, "\n"))
	}
}

func workstateInputLiterals(t *testing.T, repoRoot, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if isWorkstateInputType(lit.Type) {
			rel, _ := filepath.Rel(repoRoot, path)
			pos := fset.Position(lit.Pos())
			out = append(out, "  "+rel+":"+itoa(pos.Line))
		}
		return true
	})
	return out
}

func isWorkstateInputType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "WorkstateInput"
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == "WorkstateInput"
	default:
		return false
	}
}
