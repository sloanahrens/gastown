package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func TestIsKnownSession_UsesRegistryAndHQPrefix(t *testing.T) {
	t.Parallel()
	r := NewPrefixRegistry()
	r.Register("xy", "xrig")

	if !r.IsKnownSession("hq-deacon") {
		t.Fatal("expected hq-deacon to always be known")
	}
	if !r.IsKnownSession("xy-worker") {
		t.Fatal("expected xy-worker to be known via registry prefix")
	}
	if r.IsKnownSession("zz-worker") {
		t.Fatal("expected zz-worker to be unknown")
	}
}

func TestLoadRegistryDoesNotLoadAgentRegistryGlobally(t *testing.T) {
	t.Parallel()
	// InitRegistry used to merge the town's settings/agents.json into a
	// process-global agent registry; rig files merged later leaked into every
	// other rig (gt-rg4f1). It now only reports a malformed file: agents are
	// resolved against config.AgentRegistryFor(town, rig).

	townRoot := t.TempDir()

	// Create settings/agents.json with a process_names override.
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	registry := config.AgentRegistry{
		Version: config.CurrentAgentRegistryVersion,
		Agents: map[string]*config.AgentPresetInfo{
			"claude": {
				Name:         "claude",
				Command:      "claude",
				Args:         []string{"--dangerously-skip-permissions"},
				ProcessNames: []string{"node", "claude", ".claude-unwrapped"},
			},
		},
	}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "agents.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRegistry(townRoot); err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}

	if got := config.GetProcessNames("claude"); len(got) != 2 {
		t.Fatalf("GetProcessNames(claude) = %v after LoadRegistry, want the built-in [node claude]", got)
	}
}

func TestLoadRegistryReportsMalformedAgentRegistry(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "agents.json"), []byte("{malformed"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRegistry(townRoot); err == nil {
		t.Fatal("LoadRegistry with malformed agents.json: want an error, got nil")
	}
}

func TestLoadRegistryNoAgentsJSON(t *testing.T) {
	t.Parallel()
	// LoadRegistry must not fail when settings/agents.json is absent.

	townRoot := t.TempDir()

	if _, err := LoadRegistry(townRoot); err != nil {
		t.Fatalf("LoadRegistry with no agents.json: %v", err)
	}
}
