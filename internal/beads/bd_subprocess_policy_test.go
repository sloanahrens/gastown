package beads

import (
	"fmt"
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
	"internal/plugin",
}

// policyConstructors are the bd command builders hardenedPackages may use: the
// ones that apply ConfigureCommand's env targeting, read-only routing and
// detached process group. A constructor absent from this map is denied to
// hardenedPackages, so a pass-through fails closed rather than silently widening
// their reach. (gt-sz0s)
//
// Empty, because no constructor applies that policy any more: the four that did
// went with gt-7iwy0.4.12, and every builder left is an env pass-through that
// would skip the routing a gate-driving package's reads depend on. A hardened
// package reaches bd through beads.Client. (gt-vlw61)
var policyConstructors = map[string]bool{}

// passThroughConstructors are the bd builders code outside internal/beads may
// use. They apply machine mode and nothing else — a caller supplies dir and env,
// so no env targeting and no process group — which is why hardenedPackages are
// not among their callers. (gt-vlw61)
var passThroughConstructors = map[string]bool{
	"CommandWithEnv":         true,
	"CommandContextWithEnv":  true,
	"CommandWithPath":        true,
	"CommandContextWithPath": true,
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
		t.Fatalf("hardened packages reach bd through beads.Client; these calls spawn bd without the env targeting, read-only routing and detached process group their gate decisions need (gt-sz0s):\n%s", strings.Join(violations, "\n"))
	}
}

// TestPassThroughConstructorsMatchExec fails when passThroughConstructors and the
// exported func Command* declarations in exec.go disagree, so renaming or
// deleting a constructor breaks this test rather than leaving a dead name in the
// guidance (gt-vlw61).
func TestPassThroughConstructorsMatchExec(t *testing.T) {
	declared := exportedCommandFuncs(t, filepath.Join(repoRootFromCaller(t), "internal", "beads", "exec.go"))
	if fmt.Sprint(sortedNames(declared)) != fmt.Sprint(sortedNames(passThroughConstructors)) {
		t.Errorf("exec.go declares func Command* %v, but the guidance names %v",
			sortedNames(declared), sortedNames(passThroughConstructors))
	}
}

// exportedCommandFuncs returns the exported func Command* names path declares.
func exportedCommandFuncs(t *testing.T, path string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	found := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !ast.IsExported(fn.Name.Name) {
			continue
		}
		if strings.HasPrefix(fn.Name.Name, "Command") {
			found[fn.Name.Name] = true
		}
	}
	return found
}

// TestBdSubprocessPolicyDrivesTheFailingBranch scans a package body the test
// writes itself: a pass-through constructor is reported and a Client call is
// not, so both the empty allowlist and the Client route the caller is meant to
// take are exercised (gt-vlw61).
func TestBdSubprocessPolicyDrivesTheFailingBranch(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "pass-through constructor is denied",
			src:  "package p\n\nfunc f() { beads.CommandWithPath(\"bd\", \"\", nil, \"list\") }",
			want: []string{"p.go:3"},
		},
		{
			name: "Client call is allowed",
			src:  "package p\n\nfunc f(c beads.Client) { _, _ = c.List(beads.ListOptions{}) }",
			want: nil,
		},
		{
			name: "hand-rolled bd subprocess is denied",
			src:  "package p\n\nfunc f() { exec.Command(\"bd\", \"list\") }",
			want: []string{"p.go:3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "p.go")
			if err := os.WriteFile(path, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			got := scanBDSubprocesses(t, dir, path, isBypassOfPolicy)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("violations = %v, want %v", got, tc.want)
			}
		})
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
		t.Fatalf("build bd subprocesses outside internal/beads with %s instead of a hand-rolled command; these pass-throughs apply machine mode but no environment policy (gt-sz0s):\n%s",
			strings.Join(sortedNames(passThroughConstructors), "/"), strings.Join(violations, "\n"))
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
