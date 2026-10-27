//go:build integration

package repoguard

import (
	"path/filepath"
	"testing"
)

// TestIntegrationPluginRunShTree holds this checkout's plugins/*/run.sh to
// internal/repoguard/plugin-run-sh.txt, with each shim judged against the
// binary its plugin.md declares.
//
// It reads the checkout, so it lives in the integration tier: the unit tier
// runs TestPluginRunShTree over a tree staged in t.TempDir instead, and
// docs/testing.md keeps the two apart. The integration tier is where the
// checkout's own three scripts are held, so the allowlist cannot drift from the
// tree without this test failing (gt-tpg0p.2).
func TestIntegrationPluginRunShTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	allowed, err := ReadPluginRunShAllowlist(filepath.Join(root, "internal", "repoguard", "plugin-run-sh.txt"))
	if err != nil {
		t.Fatalf("reading plugin-run-sh.txt: %v", err)
	}
	findings, err := CheckPluginRunSh(PluginRunShTree(root), allowed)
	if err != nil {
		t.Fatalf("listing plugins/*/run.sh: %v", err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}
