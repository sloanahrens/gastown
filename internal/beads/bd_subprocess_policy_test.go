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

// hardenedPackages route their bd subprocesses through the policy constructors
// because their reads drive gate decisions. (gt-sz0s)
var hardenedPackages = []string{
	"internal/deacon",
	"internal/plugin",
	"internal/refinery",
	"internal/witness",
}

// policyConstructors apply ConfigureCommand: env targeting, read-only routing
// and a detached process group. A constructor absent from this map is denied to
// hardenedPackages, so a new pass-through fails closed rather than silently
// widening their reach. (gt-sz0s)
var policyConstructors = map[string]bool{
	"Command":               true,
	"CommandContext":        true,
	"CommandContextBounded": true,
	"CommandContextWithBin": true,
}

// TestBdSubprocessPolicyInHardenedPackages requires hardenedPackages to reach bd
// only through policyConstructors.
func TestBdSubprocessPolicyInHardenedPackages(t *testing.T) {
	repoRoot := repoRootFromCaller(t)

	var violations []string
	for _, pkg := range hardenedPackages {
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
			path := filepath.Join(dir, name)
			violations = append(violations, scanBDSubprocesses(t, repoRoot, path, isBypassOfPolicy)...)
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("spawn bd through %s in hardened packages; the pass-through constructors skip env targeting, read-only mode and the detached process group (gt-sz0s):\n%s",
			strings.Join(sortedNames(policyConstructors), "/"), strings.Join(violations, "\n"))
	}
}

func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestNoAdHocBdSubprocessesOutsideBeads scans every package outside the
// constructors' own implementation for a hand-built bd exec.Cmd.
func TestNoAdHocBdSubprocessesOutsideBeads(t *testing.T) {
	repoRoot := repoRootFromCaller(t)

	var violations []string
	err := filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, relErr := filepath.Rel(repoRoot, path)
			// internal/testutil holds the equivalent, deliberately separate helper.
			if relErr == nil && (rel == "internal/beads" || rel == "internal/testutil") {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		violations = append(violations, scanBDSubprocesses(t, repoRoot, path, isAdHocBDSubprocess)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("do not spawn bd directly outside internal/beads; use Command/CommandContext/CommandContextWithBin for the environment policy, or CommandWithEnv/CommandContextWithEnv/CommandWithPath to supply your own dir and env (gt-sz0s):\n%s", strings.Join(violations, "\n"))
	}
}

func repoRootFromCaller(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// scanBDSubprocesses reports every call in path that flag returns true for.
// Names are tracked per function, so two unrelated locals called bin in one
// file do not be mistaken for each other.
func scanBDSubprocesses(t *testing.T, repoRoot, path string, flag func(*ast.CallExpr, map[string]bool) bool) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		named := bdNamedIdents(fn.Body)
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok || !flag(call, named) {
				return true
			}
			pos := fset.Position(call.Pos())
			rel, relErr := filepath.Rel(repoRoot, pos.Filename)
			if relErr != nil {
				rel = pos.Filename
			}
			out = append(out, rel+":"+strconv.Itoa(pos.Line))
			return true
		})
		return false
	})
	return out
}

// bdNamedIdents returns the identifiers within body that hold the bd binary,
// following local assignments such as bin := f.bdBin or bin = "bd".
func bdNamedIdents(body *ast.BlockStmt) map[string]bool {
	named := map[string]bool{}
	for changed := true; changed; {
		changed = false
		ast.Inspect(body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != len(assign.Rhs) {
				return true
			}
			for i, lhs := range assign.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || named[ident.Name] {
					continue
				}
				if bdNamedExpr(assign.Rhs[i], named) {
					named[ident.Name] = true
					changed = true
				}
			}
			return true
		})
	}
	return named
}

// bdNamedExpr reports whether expr yields the bd binary.
func bdNamedExpr(expr ast.Expr, named map[string]bool) bool {
	switch v := expr.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING && unquoteString(v.Value) == "bd"
	case *ast.Ident:
		return isBDCommandName(v.Name) || named[v.Name]
	case *ast.SelectorExpr:
		return isBDCommandName(v.Sel.Name) || named[v.Sel.Name]
	default:
		return false
	}
}

// isBypassOfPolicy matches a bd subprocess that skips the environment policy.
func isBypassOfPolicy(call *ast.CallExpr, named map[string]bool) bool {
	if isAdHocBDSubprocess(call, named) {
		return true
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "beads" {
		return false
	}
	if !bdCommandConstructor(sel.Sel.Name) {
		return false
	}
	return !policyConstructors[sel.Sel.Name]
}

// bdCommandConstructor reports whether name is one of this package's bd
// command builders.
func bdCommandConstructor(name string) bool {
	return strings.HasPrefix(name, "Command") || name == "ConfigureCommand"
}

// isAdHocBDSubprocess matches exec.Command/exec.CommandContext spawning the bd
// binary, including through a local renamed from a bd-named variable.
func isAdHocBDSubprocess(call *ast.CallExpr, named map[string]bool) bool {
	name, ok := calledName(call)
	if !ok || (name != "Command" && name != "CommandContext") {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "exec" {
		return false
	}
	argIndex := 0
	if name == "CommandContext" {
		argIndex = 1
	}
	return len(call.Args) > argIndex && isBDCommandArg(call.Args[argIndex], named)
}

// calledName returns the final identifier of a call's function expression.
func calledName(call *ast.CallExpr) (string, bool) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name, true
	case *ast.SelectorExpr:
		return fun.Sel.Name, true
	default:
		return "", false
	}
}

func isBDCommandArg(expr ast.Expr, named map[string]bool) bool {
	switch v := expr.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING && unquoteString(v.Value) == "bd"
	case *ast.Ident:
		return isBDCommandName(v.Name) || named[v.Name]
	case *ast.SelectorExpr:
		return isBDCommandName(v.Sel.Name) || named[v.Sel.Name]
	default:
		return false
	}
}

// isBDCommandName reports whether a variable or field name denotes the bd
// binary.
func isBDCommandName(name string) bool {
	switch strings.ToLower(name) {
	case "bd", "bdpath", "bdbin":
		return true
	default:
		return false
	}
}

func unquoteString(lit string) string {
	value, err := strconv.Unquote(lit)
	if err != nil {
		return ""
	}
	return value
}
