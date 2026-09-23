package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// allowedPresetLookups names the only call sites outside internal/config that
// may call GetAgentPresetByName: each passes a built-in/registry or
// already-resolved harness name, never a custom agent name. Key: file (relative
// to internal/) + "#" + enclosing function. Anything else must use
// ResolveAgentPreset (claude-9a8).
var allowedPresetLookups = map[string]bool{
	"cmd/seance.go#resolveSeanceCommand": true,
	"cmd/config.go#runConfigAgentList":   true,
	// runConfigAgentGet checks town custom agents before this built-in lookup.
	"cmd/config.go#runConfigAgentGet":          true,
	"runtime/runtime.go#EnsureSettingsForRole": true,
	"crew/manager.go#buildResumeArgs":          true,
	"crew/manager.go#Start":                    true,
	// provision.go receives harness names only: runtime passes the hooks
	// provider and rig/manager resolves default_agent first (claude-9a8).
	"templates/commands/provision.go#getAgentConfigDir": true,
}

func TestNoAgentNamePresetLookups(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..")
	fset := token.NewFileSet()
	var bad []string
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filepath.Base(path) == "config" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "GetAgentPresetByName" {
					return true
				}
				key := filepath.ToSlash(rel) + "#" + fn.Name.Name
				seen[key] = true
				if !allowedPresetLookups[key] {
					bad = append(bad, key+" ("+fset.Position(call.Pos()).String()+")")
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for key := range allowedPresetLookups {
		if !seen[key] {
			t.Errorf("allowedPresetLookups entry %q matches no call; remove it so it cannot silently allow a future lookup", key)
		}
	}
	for _, b := range bad {
		t.Errorf("GetAgentPresetByName called with a possibly custom agent name at %s; use ResolveAgentPreset (claude-9a8), or add to allowedPresetLookups if the name is always a harness name", b)
	}
}
