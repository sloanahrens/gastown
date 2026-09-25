package rig

import (
	"os"
	"path/filepath"
	"testing"
)

func writeRigRootConfig(t *testing.T, townRoot, body string) {
	t.Helper()
	rigDir := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveWitnessSessionConfig(t *testing.T) {
	townRoot := t.TempDir()
	if cfg := ResolveWitnessSessionConfig(townRoot, "gastown"); cfg != nil {
		t.Fatalf("no config.json = %+v, want nil (off)", cfg)
	}

	writeRigRootConfig(t, townRoot, `{"type":"rig","version":1,"name":"gastown"}`)
	cfg := ResolveWitnessSessionConfig(townRoot, "gastown")
	if cfg != nil {
		t.Fatalf("no witness block = %+v, want nil (off)", cfg)
	}
	if cfg.MinCycles() != 3 || cfg.MaxCycles() != 8 {
		t.Fatalf("nil defaults = %d/%d, want 3/8", cfg.MinCycles(), cfg.MaxCycles())
	}

	writeRigRootConfig(t, townRoot, `{"type":"rig","version":1,"name":"gastown",
  "witness": {"cycle_session_at_idle_cap": true}}`)
	cfg = ResolveWitnessSessionConfig(townRoot, "gastown")
	if cfg == nil || !cfg.CycleSessionAtIdleCap || cfg.MinCycles() != 3 || cfg.MaxCycles() != 8 {
		t.Fatalf("flag on = %+v, want on with defaults 3/8", cfg)
	}

	writeRigRootConfig(t, townRoot, `{"type":"rig","version":1,"name":"gastown",
  "witness": {"cycle_session_at_idle_cap": true, "cycle_session_min_cycles": 4, "cycle_session_max_cycles": -1}}`)
	cfg = ResolveWitnessSessionConfig(townRoot, "gastown")
	if cfg.MinCycles() != 4 || cfg.MaxCycles() != 0 {
		t.Fatalf("overrides = %d/%d, want 4 and backstop disabled", cfg.MinCycles(), cfg.MaxCycles())
	}
}
