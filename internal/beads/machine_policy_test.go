package beads

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// machineExemptCallers are the only functions that run a bd call outside
// machine mode, and why. A caller whose stdout is parsed belongs in machine
// mode instead: convert it to read the envelope's data (LegacyPayload).
var machineExemptCallers = map[string]string{
	"internal/beads/exec.go:policyEnv":            "bd sql (machineExempt): machine mode sorts the row keys, losing the SELECT order the csv readers index by",
	"internal/beads/machine.go:machineEnvForCall": "bd sql (machineExempt)",
	"internal/beads/beads.go:runWithStdin":        "bd sql (machineExempt)",
	"internal/beads/beads.go:runWithRouting":      "bd sql (machineExempt)",
	"internal/cmd/show.go:newBdShowInvocation":    "gt show replaces the process with bd show, which prints for the operator's terminal",
	"internal/cmd/formula.go:passBdFormulaOutput": "gt formula list/show without --json prints bd's prose for the operator's terminal",
}

// TestMachineModeOptOutsAreEnumerated fails on a new way out of machine mode:
// a call to WithoutMachineEnv outside machineExemptCallers, or a hand-written
// BD_MACHINE value other than 1. The environment policy gives every bd
// subprocess machine mode (gt-fd2cu.5); an opt-out is a decision to make in
// review, not a line to add quietly.
func TestMachineModeOptOutsAreEnumerated(t *testing.T) {
	repoRoot := repoRootFromCaller(t)

	var found []string
	var badValues []string
	err := filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, _ := filepath.Rel(repoRoot, path)
		rel = filepath.ToSlash(rel)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// The definition itself is not a caller.
			if fn.Name.Name == "WithoutMachineEnv" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if calleeName(call.Fun) == "WithoutMachineEnv" {
					found = append(found, rel+":"+fn.Name.Name)
				}
				return true
			})
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(s, "BD_MACHINE=") && s != "BD_MACHINE=1" {
				badValues = append(badValues, rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line)+" "+s)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}

	seen := map[string]bool{}
	for _, site := range found {
		seen[site] = true
		if _, ok := machineExemptCallers[site]; !ok {
			t.Errorf("%s calls WithoutMachineEnv: every gastown bd call runs in machine mode (gt-fd2cu.5). Read the payload with LegacyPayload, or add the caller to machineExemptCallers with the reason its stdout cannot be JSON", site)
		}
	}
	var stale []string
	for site := range machineExemptCallers {
		if !seen[site] {
			stale = append(stale, site)
		}
	}
	sort.Strings(stale)
	for _, site := range stale {
		t.Errorf("machineExemptCallers names %s, which no longer calls WithoutMachineEnv: delete the entry", site)
	}
	sort.Strings(badValues)
	for _, v := range badValues {
		t.Errorf("%s: BD_MACHINE is set to something other than 1; leave machine mode to WithoutMachineEnv", v)
	}
}

// calleeName is the last identifier of a call's function expression.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
