package beads

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// agentBeadIdentRE matches expressions that carry an agent-bead ID.
var agentBeadIdentRE = regexp.MustCompile(`(?i)agentbead|agentid\b|witnessbeadid|refinerybeadid|polecatbeadid|crewbeadid`)

// agentBeadHelpers are the Beads methods that must only be called on an
// agent-scoped or pinned wrapper — never chained directly onto beads.New*().
var agentBeadHelpers = map[string]bool{
	"UpdateAgentState": true, "UpdateAgentCleanupStatus": true,
	"UpdateAgentDescriptionFields": true, "CreateAgentBead": true,
	"CreateOrReopenAgentBead": true, "ResetAgentBeadForReuse": true,
	"ListAgentBeads": true, "GetAgentBead": true,
}

// TestNoShellBdWritesToAgentBeads is the gt-a6g structural guard. Agent beads
// live in exactly one database per ID; the only code allowed to decide which
// database is internal/beads. A shell `bd update <agentBeadID>` from an
// arbitrary cwd resolves local-first and wrote the legacy town row for months
// (hq-kt9y1). This test fails the build when that pattern reappears.
func TestNoShellBdWritesToAgentBeads(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	packages := []string{
		"internal/cmd", "internal/witness", "internal/refinery", "internal/daemon",
		"internal/polecat", "internal/doctor", "internal/deacon", "internal/dog",
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
			src, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			violations = append(violations, agentBeadShellWrites(t, filepath.Join(pkg, name), src)...)
		}
	}
	if len(violations) > 0 {
		t.Fatalf("agent beads must be read/written through beads.New(...).ForAgentBead() or beads.NewRigLocal(...), never via shell bd or a bare beads.New*() chain:\n%s", strings.Join(violations, "\n"))
	}
}

// agentBeadShellWrites returns one line per violation in src.
func agentBeadShellWrites(t *testing.T, label string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, label, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", label, err)
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		pos := fset.Position(call.Pos())
		// (a) shell bd invocations carrying an agent-bead identifier.
		if isShellBdCall(call) {
			for _, arg := range call.Args {
				if agentBeadIdentRE.MatchString(exprString(arg)) {
					out = append(out, pos.String()+": shell bd call with agent-bead argument "+exprString(arg))
					break
				}
			}
		}
		// (b) agent-bead helper chained directly onto beads.New*(...).
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && agentBeadHelpers[sel.Sel.Name] {
			if recv, ok := sel.X.(*ast.CallExpr); ok {
				if rs, ok := recv.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := rs.X.(*ast.Ident); ok && pkg.Name == "beads" && strings.HasPrefix(rs.Sel.Name, "New") && rs.Sel.Name != "NewRigLocal" {
						out = append(out, pos.String()+": "+sel.Sel.Name+" called directly on beads."+rs.Sel.Name+"(...); insert .ForAgentBead()")
					}
				}
			}
		}
		return true
	})
	return out
}

func isShellBdCall(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == "BdCmd"
	case *ast.SelectorExpr:
		if x, ok := fun.X.(*ast.Ident); ok {
			if x.Name == "bd" && (fun.Sel.Name == "Run" || fun.Sel.Name == "Exec") {
				return true
			}
			if x.Name == "exec" && fun.Sel.Name == "Command" && len(call.Args) > 0 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"bd"` {
					return true
				}
			}
		}
	}
	return false
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprString(v.Fun) + "()"
	case *ast.BasicLit:
		return v.Value
	default:
		return ""
	}
}

// TestAgentBeadGuardDetectsEachPattern proves the guard is not vacuous.
func TestAgentBeadGuardDetectsEachPattern(t *testing.T) {
	cases := map[string]string{
		"bd.Run":       `package x; func f(bd *BdCli, workDir, agentBeadID string) { _ = bd.Run(workDir, "update", agentBeadID, "--description", "d") }`,
		"BdCmd":        `package x; func f(agentID string) { _ = BdCmd("update", agentID, "--status=open") }`,
		"exec.Command": `package x; import "os/exec"; func f(witnessBeadID string) { _ = exec.Command("bd", "close", witnessBeadID) }`,
		"bare chain":   `package x; func f(id string) { _ = beads.New("/x").UpdateAgentState(id, "idle") }`,
	}
	for name, src := range cases {
		if v := agentBeadShellWrites(t, name+".go", []byte(src)); len(v) == 0 {
			t.Errorf("%s: guard did not flag %q", name, src)
		}
	}
	clean := `package x; func f(id string) { _ = beads.New("/x").ForAgentBead().UpdateAgentState(id, "idle"); _ = beads.NewRigLocal("/x").GetAgentBead(id) }`
	if v := agentBeadShellWrites(t, "clean.go", []byte(clean)); len(v) != 0 {
		t.Errorf("guard flagged compliant code: %v", v)
	}
}
