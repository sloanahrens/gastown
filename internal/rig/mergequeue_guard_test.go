package rig

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

// TestNoMergeSettingsCommandCallsOutsideResolver is the gt-egiv structural
// regression test. gt-me9t introduced the rig-root config.json merge_queue
// floor and wired it into loadRigCommandVars, but four other gate-command
// resolution sites (buildRefineryPatrolVars, resolveSetupCommand, mq_submit,
// done's --pre-verified guard) kept their own settings/config.json-only
// merge. gt-k4sy and gt-egiv unified all five behind one resolver,
// ResolveMergeQueueConfig in this file — but nothing stopped a sixth site
// from being added the old way, duplicating the merge and drifting again
// the moment a new tier is introduced.
//
// This test enforces the structural fix: config.MergeSettingsCommand, the
// overlay primitive the three-tier merge is built from, may only be called
// from ResolveMergeQueueConfig in this file (and from internal/config's own
// tests, which exercise the primitive directly). Any other production call
// site is a second, independently-drifting resolver — the exact class of
// bug gt-egiv exists to close.
func TestNoMergeSettingsCommandCallsOutsideResolver(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	internalDir := filepath.Join(repoRoot, "internal")

	// The only production file allowed to call MergeSettingsCommand: the
	// canonical resolver's own home.
	allowedFile := filepath.Join(internalDir, "rig", "manager.go")

	var violations []string
	err := filepath.WalkDir(internalDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if path == allowedFile {
			return nil
		}
		violations = append(violations, mergeSettingsCommandCalls(t, repoRoot, path)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", internalDir, err)
	}

	if len(violations) > 0 {
		t.Fatalf("do not call MergeSettingsCommand outside rig.ResolveMergeQueueConfig (internal/rig/manager.go); route the new call site through rig.ResolveMergeQueueConfig(townRoot, rigName) instead, so the rig-root -> repo -> rig-local precedence lives in exactly one place:\n%s", strings.Join(violations, "\n"))
	}
}

func mergeSettingsCommandCalls(t *testing.T, repoRoot, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isMergeSettingsCommandCallee(call.Fun) {
			rel, _ := filepath.Rel(repoRoot, path)
			pos := fset.Position(call.Pos())
			out = append(out, "  "+rel+":"+itoa(pos.Line))
		}
		return true
	})
	return out
}

func isMergeSettingsCommandCallee(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "MergeSettingsCommand"
	case *ast.SelectorExpr:
		return e.Sel != nil && e.Sel.Name == "MergeSettingsCommand"
	default:
		return false
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
