package tmux

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTownAgentsJSON writes <townRoot>/settings/agents.json.
func writeTownAgentsJSON(t *testing.T, townRoot, content string) {
	t.Helper()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A legacy session (GT_AGENT but no GT_PROCESS_NAMES) is checked against the
// process names of its own town's settings/agents.json. On NixOS the claude
// binary runs as ".claude-unwrapped", which only the override lists; before
// gt-rg4f1 this came from a process-global registry that other rigs could
// overwrite.
func TestIsAgentAliveLegacyFallbackUsesTownRegistry(t *testing.T) {
	t.Parallel()
	nixTown := t.TempDir()
	writeTownAgentsJSON(t, nixTown, `{"version":1,"agents":{"claude":{"process_names":["node","claude",".claude-unwrapped"]}}}`)
	plainTown := t.TempDir()

	f := newFakeServer()
	nix := f.addSession("gt-nix", ".claude-unwrapped")
	plain := f.addSession("gt-plain", ".claude-unwrapped")
	f.with(func() {
		nix.env["GT_AGENT"] = "claude"
		nix.env["GT_TOWN_ROOT"] = nixTown
		plain.env["GT_AGENT"] = "claude"
		plain.env["GT_TOWN_ROOT"] = plainTown
	})
	tm, _ := f.tmux(nil)
	tm.getenv = func(string) string { return "" }

	if alive, err := tm.IsAgentAliveChecked("gt-nix"); err != nil || !alive {
		t.Errorf("gt-nix: want alive via its town's claude process_names override (err=%v)", err)
	}
	if alive, err := tm.IsAgentAliveChecked("gt-plain"); err != nil || alive {
		t.Errorf("gt-plain: want not alive; its town has no override, so another town's must not apply (err=%v)", err)
	}
}
