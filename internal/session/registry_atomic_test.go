package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func TestDefaultRegistrySwapAndPrefixFor(t *testing.T) {
	old := DefaultRegistry()
	defer SetDefaultRegistry(old)

	r := NewPrefixRegistry()
	r.Register("xy", "xrig")
	SetDefaultRegistry(r)

	if got := DefaultRegistry(); got != r {
		t.Fatalf("DefaultRegistry() did not return swapped registry")
	}
	if got := PrefixFor("xrig"); got != "xy" {
		t.Fatalf("PrefixFor(xrig) = %q, want %q", got, "xy")
	}
	if got := PrefixFor("unknown-rig"); got != DefaultPrefix {
		t.Fatalf("PrefixFor(unknown-rig) = %q, want %q", got, DefaultPrefix)
	}
}

func TestIsKnownSession_UsesDefaultRegistryAndHQPrefix(t *testing.T) {
	old := DefaultRegistry()
	defer SetDefaultRegistry(old)

	r := NewPrefixRegistry()
	r.Register("xy", "xrig")
	SetDefaultRegistry(r)

	if !IsKnownSession("hq-mayor") {
		t.Fatal("expected hq-mayor to always be known")
	}
	if !IsKnownSession("xy-worker") {
		t.Fatal("expected xy-worker to be known via registry prefix")
	}
	if IsKnownSession("zz-worker") {
		t.Fatal("expected zz-worker to be unknown")
	}
}

func TestInitRegistryDoesNotLoadAgentRegistryGlobally(t *testing.T) {
	// InitRegistry used to merge the town's settings/agents.json into a
	// process-global agent registry; rig files merged later leaked into every
	// other rig (gt-rg4f1). It now only reports a malformed file: agents are
	// resolved against config.AgentRegistryFor(town, rig).
	//
	// NOTE: cannot use t.Parallel() — mutates the global prefix registry.
	old := DefaultRegistry()
	defer SetDefaultRegistry(old)

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

	if err := InitRegistry(townRoot); err != nil {
		t.Fatalf("InitRegistry: %v", err)
	}

	if got := config.GetProcessNames("claude"); len(got) != 2 {
		t.Fatalf("GetProcessNames(claude) = %v after InitRegistry, want the built-in [node claude]", got)
	}
}

func TestInitRegistryReportsMalformedAgentRegistry(t *testing.T) {
	// NOTE: cannot use t.Parallel() — mutates the global prefix registry.
	old := DefaultRegistry()
	defer SetDefaultRegistry(old)

	townRoot := t.TempDir()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "agents.json"), []byte("{malformed"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := InitRegistry(townRoot); err == nil {
		t.Fatal("InitRegistry with malformed agents.json: want an error, got nil")
	}
}

func TestInitRegistryNoAgentsJSON(t *testing.T) {
	// InitRegistry must not fail when settings/agents.json is absent.
	old := DefaultRegistry()
	defer SetDefaultRegistry(old)

	townRoot := t.TempDir()

	if err := InitRegistry(townRoot); err != nil {
		t.Fatalf("InitRegistry with no agents.json: %v", err)
	}
}
