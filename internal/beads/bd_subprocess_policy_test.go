package beads

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// legacyAdHocBDSubprocesses is a ratchet, not an allowlist to add to: it names
// every file outside internal/beads that still builds a bd exec.Cmd by hand
// instead of going through Command/CommandContext/CommandWithEnv/
// CommandWithPath (or their CommandContext counterparts), together with the
// exact number of such call sites still in that file as of gt-sz0s. A file
// converted since is expected to drop out of this map entirely — leaving a
// stale higher count here would silently stop catching a regression back up
// to it. gt-sz0s is the tracking bead for finishing this migration; see its
// notes for the remaining packages.
//
// Do not add a new entry, or raise an existing count, to make this test pass:
// that defeats its purpose. New code should call the centralized
// constructors from the start.
var legacyAdHocBDSubprocesses = map[string]int{}

// TestNoAdHocBdSubprocessesOutsideBeads is the repo-wide version of the
// hardened-package check: two independent MRs in one night (gt-wisp-nysy and
// gt-wisp-a74, see gt-sz0s) hand-rolled a bd exec.Cmd outside internal/beads
// and each shipped its own env/error-handling bug that the centralized
// constructors already avoid. Rather than re-listing "hardened" packages one
// at a time as each gets bitten, this scans every package outside
// internal/beads (the constructors' own implementation) and internal/testutil
// (the equivalent, deliberately separate, sanctioned helper for tests — see
// its doc comment) and fails on any ad hoc bd subprocess beyond the tracked
// legacy count in legacyAdHocBDSubprocesses. A brand new file starts at zero
// tolerance automatically, since it has no entry in that map.
func TestNoAdHocBdSubprocessesOutsideBeads(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

	counts := map[string][]string{}
	err := filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr == nil && (rel == "internal/beads" || rel == "internal/testutil") {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			rel = path
		}
		for _, loc := range adHocBDSubprocesses(t, repoRoot, path) {
			counts[rel] = append(counts[rel], loc)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}

	var violations []string
	for rel, locs := range counts {
		allowed := legacyAdHocBDSubprocesses[rel]
		if len(locs) > allowed {
			violations = append(violations, locs...)
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("do not spawn bd directly outside internal/beads; use Command/CommandContext/CommandWithEnv/CommandContextWithEnv/CommandWithPath/CommandContextWithPath so env targeting, read-only mode, and side-effect suppression stay centralized (gt-sz0s):\n%s", strings.Join(violations, "\n"))
	}
}

func adHocBDSubprocesses(t *testing.T, repoRoot, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isExecCommandCall(call) {
			return true
		}
		argIndex := 0
		if selectorName(call) == "CommandContext" {
			argIndex = 1
		}
		if len(call.Args) <= argIndex || !isBDCommandArg(call.Args[argIndex]) {
			return true
		}
		pos := fset.Position(call.Pos())
		rel, err := filepath.Rel(repoRoot, pos.Filename)
		if err != nil {
			rel = pos.Filename
		}
		out = append(out, rel+":"+strconv.Itoa(pos.Line))
		return true
	})
	return out
}

func isExecCommandCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext") {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "exec"
}

func selectorName(call *ast.CallExpr) string {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	return ""
}

func isBDCommandArg(expr ast.Expr) bool {
	switch v := expr.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return false
		}
		value, err := strconv.Unquote(v.Value)
		return err == nil && value == "bd"
	case *ast.Ident:
		return strings.EqualFold(v.Name, "bdPath")
	case *ast.SelectorExpr:
		return strings.EqualFold(v.Sel.Name, "bdPath")
	default:
		return false
	}
}
