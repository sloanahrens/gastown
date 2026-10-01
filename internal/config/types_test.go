package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- ParseDurationOrDefault ---

func TestParseDurationOrDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		fallback time.Duration
		want     time.Duration
	}{
		{"valid seconds", "15s", 0, 15 * time.Second},
		{"valid minutes", "5m", 0, 5 * time.Minute},
		{"valid hours", "2h", 0, 2 * time.Hour},
		{"valid milliseconds", "500ms", 0, 500 * time.Millisecond},
		{"valid composite", "1m30s", 0, 90 * time.Second},
		{"empty string returns fallback", "", 42 * time.Second, 42 * time.Second},
		{"invalid string returns fallback", "not-a-duration", 7 * time.Second, 7 * time.Second},
		{"negative duration parses", "-5s", 10 * time.Second, -5 * time.Second},
		{"zero duration parses", "0s", 10 * time.Second, 0},
		{"bare number returns fallback", "15", 3 * time.Second, 3 * time.Second},
		{"whitespace returns fallback", "  ", 1 * time.Second, 1 * time.Second},
		{"zero fallback with empty", "", 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ParseDurationOrDefault(tt.input, tt.fallback)
			if got != tt.want {
				t.Errorf("ParseDurationOrDefault(%q, %v) = %v, want %v",
					tt.input, tt.fallback, got, tt.want)
			}
		})
	}
}

// --- Gemini provider defaults ---

func TestTownSettings_WithoutNewFields_LoadsDefaults(t *testing.T) {
	t.Parallel()
	// Simulate a pre-existing settings/config.json that has NO new config fields.
	// This verifies backward compatibility: existing deployments continue to work.
	settingsJSON := `{
		"type": "town-settings",
		"version": 1,
		"default_agent": "claude"
	}`

	tmpDir := t.TempDir()
	settingsDir := filepath.Join(tmpDir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	settingsPath := filepath.Join(settingsDir, "config.json")
	if err := os.WriteFile(settingsPath, []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ts, err := LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}

	// Existing fields should still load correctly
	if ts.DefaultAgent != "claude" {
		t.Errorf("DefaultAgent = %q, want %q", ts.DefaultAgent, "claude")
	}
}

// TestTownSettings_RemovedFeedCuratorKeyStillLoads: towns written before the
// feed curator was deleted (gt-3vdcx) carry a feed_curator section; strict
// decoding still accepts it.
func TestTownSettings_RemovedFeedCuratorKeyStillLoads(t *testing.T) {
	t.Parallel()
	settingsJSON := `{
		"type": "town-settings",
		"version": 1,
		"default_agent": "claude",
		"feed_curator": {
			"done_dedupe_window": "25s"
		}
	}`

	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(settingsPath, []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ts, err := LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}
	if ts.DefaultAgent != "claude" {
		t.Errorf("DefaultAgent = %q, want %q", ts.DefaultAgent, "claude")
	}
}

func TestTownSettings_MissingFile_ReturnsDefaults(t *testing.T) {
	t.Parallel()
	// LoadOrCreateTownSettings on a missing file should return defaults.
	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, "does-not-exist.json")

	ts, err := LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}

	if ts.Type != "town-settings" {
		t.Errorf("Type = %q, want %q", ts.Type, "town-settings")
	}
	if ts.Version != CurrentTownSettingsVersion {
		t.Errorf("Version = %d, want %d", ts.Version, CurrentTownSettingsVersion)
	}
}

func TestTownSettings_DisabledPatrols_RoundTrip(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, "config.json")

	original := NewTownSettings()
	original.DisabledPatrols = []string{"doctor_dog", "compactor_dog", "witness"}

	if err := SaveTownSettings(settingsPath, original); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}

	loaded, err := LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}

	if len(loaded.DisabledPatrols) != 3 {
		t.Fatalf("expected 3 disabled patrols, got %d", len(loaded.DisabledPatrols))
	}

	expected := map[string]bool{"doctor_dog": true, "compactor_dog": true, "witness": true}
	for _, p := range loaded.DisabledPatrols {
		if !expected[p] {
			t.Errorf("unexpected disabled patrol: %q", p)
		}
	}
}

func TestTownSettings_DisabledPatrols_OmitemptyWhenNil(t *testing.T) {
	t.Parallel()
	ts := NewTownSettings()
	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "disabled_patrols") {
		t.Error("JSON should not contain disabled_patrols when nil")
	}
}

// --- PolecatPool knobs ---

// TestTownSettings_RetiredPoolKeysStillLoad: the live town config still
// carries the retired local-model seat (local_agent, max_local, idle_fill, D4)
// and the retired seat-refill policy (max_priority and the rest, gt-4k3fj.8.6).
// It must decode under strict decoding, keep the live seat (overflow_agent,
// max_overflow, min_spawn_gap), and write the retired keys back verbatim.
func TestTownSettings_RetiredPoolKeysStillLoad(t *testing.T) {
	t.Parallel()
	settingsJSON := `{
		"type": "town-settings",
		"version": 1,
		"default_agent": "claude",
		"polecat_pool": {
			"local_agent": "local-coder-polecat",
			"max_local": 0,
			"idle_fill": true,
			"max_priority": 2,
			"mode": "sling",
			"min_spawn_gap": "4m",
			"overflow_agent": "deepseek-flash",
			"max_overflow": 3
		}
	}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ts, err := LoadOrCreateTownSettings(path)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}
	pool := ts.PolecatPool
	if pool == nil || pool.OverflowAgent != "deepseek-flash" || pool.MaxOverflow != 3 || pool.MinSpawnGapD() != 4*time.Minute {
		t.Fatalf("live pool keys not loaded: %+v", pool)
	}
	if !pool.OverflowCapped() {
		t.Error("pool with max_overflow 3 must be capped")
	}

	if err := SaveTownSettings(path, ts); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"local_agent": "local-coder-polecat"`, `"max_local": 0`, `"idle_fill": true`,
		`"max_priority": 2`, `"mode": "sling"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("saved settings dropped retired key %s:\n%s", want, raw)
		}
	}
}

// TestNonClaudeProviderGetsClaudeRuntime: the Claude CLI is the only runtime
// (D4). A live town agent carries provider "deepseek" (a backend, not a
// harness) and a backend wrapper's command is not literally "claude"; both
// once took the no-hooks path and started without --settings or guards
// (gt-be0z). Both must now resolve to Claude's defaults and settings.
func TestNonClaudeProviderGetsClaudeRuntime(t *testing.T) {
	t.Parallel()
	rc := normalizeRuntimeConfig(&RuntimeConfig{Provider: "deepseek", Command: "claude"})
	if rc.Provider != string(AgentClaude) {
		t.Errorf("Provider = %q, want claude", rc.Provider)
	}
	if rc.Session.SessionIDEnv != "CLAUDE_SESSION_ID" || rc.Tmux.ReadyPromptPrefix != "❯ " || rc.Instructions.File != "CLAUDE.md" {
		t.Errorf("provider deepseek did not take Claude defaults: session=%+v tmux=%+v instructions=%+v", rc.Session, rc.Tmux, rc.Instructions)
	}

	rigPath := filepath.Join("town", "rig")
	wrapped := withRoleSettingsFlag(&RuntimeConfig{Command: "claude-deepseek-flash"}, "polecat", rigPath)
	want := []string{"--settings", filepath.Join(rigPath, "polecats", ".claude", "settings.json")}
	if len(wrapped.Args) != 2 || wrapped.Args[0] != want[0] || wrapped.Args[1] != want[1] {
		t.Errorf("backend wrapper Args = %v, want %v", wrapped.Args, want)
	}
}
