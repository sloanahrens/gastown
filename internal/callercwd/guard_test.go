package callercwd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// callercwdImportPath is the import a guard must never have: a guard reads the
// real process directory, so it cannot see the variable an agent sets.
const callercwdImportPath = "github.com/steveyegge/gastown/internal/callercwd"

// guardSourceFiles are the non-test files that enforce a rule and therefore
// must resolve the real working directory: the hook integrity check, the role
// resolver that feeds the stop guard, the stop guard itself, and workspace
// resolution (which callers include guards). The tap_guard*.go files and the
// guardlint checker are added by glob below.
var guardSourceFiles = []string{
	"internal/cmd/hook.go",        // hook worktree integrity (a hook check)
	"internal/cmd/role.go",        // GetRole feeds checkStopSlungWork (a stop guard)
	"internal/cmd/signal_stop.go", // the stop guard
	"internal/workspace/find.go",  // Find/FindFromCwd, the generic resolver
}

// TestGuardPackagesDoNotReadTheVariable pins the boundary the rework set:
// GT_CALLER_CWD may steer identity and display, never a guard or a check. A
// guard that imported this package, or mentioned the variable, could be
// steered by an agent setting its own environment (gt-yjzepk.1).
func TestGuardPackagesDoNotReadTheVariable(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	files := append([]string(nil), guardSourceFiles...)
	for _, pattern := range []string{
		"internal/cmd/tap_guard*.go", // the dangerous, pr-workflow and other command guards
		"internal/guardlint/*.go",    // the fail-open checker itself
	} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, match := range matches {
			if strings.HasSuffix(match, "_test.go") {
				continue
			}
			rel, err := filepath.Rel(root, match)
			if err != nil {
				t.Fatalf("rel %s: %v", match, err)
			}
			files = append(files, rel)
		}
	}

	for _, rel := range files {
		path := filepath.Join(root, rel)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, imp := range file.Imports {
			quoted := strings.Trim(imp.Path.Value, `"`)
			if quoted == callercwdImportPath {
				t.Errorf("%s imports %s: a guard must resolve the real process directory, and the variable may not steer it", rel, callercwdImportPath)
			}
		}
		// Reading the variable needs either this package or its name as a
		// string literal. Comments that explain the rule are not reads, so the
		// scan judges the syntax tree, not the file text.
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := node.X.(*ast.Ident); ok && x.Name == "callercwd" {
					t.Errorf("%s uses callercwd.%s: a guard must resolve the real process directory, and the variable may not steer it", rel, node.Sel.Name)
				}
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				if value, err := strconv.Unquote(node.Value); err == nil && value == EnvCallerCwd {
					t.Errorf("%s reads %s: a guard must resolve the real process directory, and the variable may not steer it", rel, EnvCallerCwd)
				}
			}
			return true
		})
	}

	// internal/guardlint/baseline.txt only shrinks: this change may not buy a
	// new exemption for the package it adds. The guardlint gate itself would
	// reject any unbaselined finding, so a line here would be a deliberate
	// exemption this bead's scope does not allow.
	baseline, err := os.ReadFile(filepath.Join(root, "internal", "guardlint", "baseline.txt"))
	if err != nil {
		t.Fatalf("read guardlint baseline: %v", err)
	}
	for i, line := range strings.Split(string(baseline), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "internal/callercwd") {
			t.Errorf("internal/guardlint/baseline.txt:%d adds an exemption for internal/callercwd; fix the code instead, the baseline only shrinks", i+1)
		}
	}
}

// repoRoot walks up from this package's directory to the module root (the
// directory holding go.mod). The scan reads repo source the way guardlint
// does; it never touches a town or HOME.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
