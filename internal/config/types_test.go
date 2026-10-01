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

func TestGeminiProviderDefaults(t *testing.T) {
	t.Parallel()

	t.Run("defaultRuntimeCommand", func(t *testing.T) {
		cmd := defaultRuntimeCommand(nil, "gemini")
		if cmd != "gemini" {
			t.Errorf("defaultRuntimeCommand(gemini) = %q, want %q", cmd, "gemini")
		}
	})

	t.Run("defaultSessionIDEnv", func(t *testing.T) {
		env := defaultSessionIDEnv(nil, "gemini")
		if env != "GEMINI_SESSION_ID" {
			t.Errorf("defaultSessionIDEnv(gemini) = %q, want %q", env, "GEMINI_SESSION_ID")
		}
	})

	t.Run("defaultHooksProvider", func(t *testing.T) {
		provider := defaultHooksProvider(nil, "gemini")
		if provider != "gemini" {
			t.Errorf("defaultHooksProvider(gemini) = %q, want %q", provider, "gemini")
		}
	})

	t.Run("defaultHooksDir", func(t *testing.T) {
		dir := defaultHooksDir(nil, "gemini")
		if dir != ".gemini" {
			t.Errorf("defaultHooksDir(gemini) = %q, want %q", dir, ".gemini")
		}
	})

	t.Run("defaultHooksFile", func(t *testing.T) {
		file := defaultHooksFile(nil, "gemini")
		if file != "settings.json" {
			t.Errorf("defaultHooksFile(gemini) = %q, want %q", file, "settings.json")
		}
	})

	t.Run("defaultProcessNames", func(t *testing.T) {
		names := defaultProcessNames(nil, "gemini", "gemini")
		if len(names) != 1 || names[0] != "gemini" {
			t.Errorf("defaultProcessNames(gemini) = %v, want [gemini]", names)
		}
	})

	t.Run("defaultReadyDelayMs", func(t *testing.T) {
		delay := defaultReadyDelayMs(nil, "gemini")
		if delay != 5000 {
			t.Errorf("defaultReadyDelayMs(gemini) = %d, want 5000", delay)
		}
	})

	t.Run("defaultInstructionsFile", func(t *testing.T) {
		file := defaultInstructionsFile(nil, "gemini")
		if file != "AGENTS.md" {
			t.Errorf("defaultInstructionsFile(gemini) = %q, want %q", file, "AGENTS.md")
		}
	})
}

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

// The idle-seat fill is the shipped behavior, so only a stored false turns it
// off; a town that has never written the knob, and a pool that does not exist,
// both keep it (gt-nn7n).
func TestPolecatPoolIdleFillEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pool *PolecatPool
		want bool
	}{
		{"no pool", nil, true},
		{"knob unset", &PolecatPool{LocalAgent: "l", MaxLocal: 3}, true},
		{"knob true", &PolecatPool{IdleFill: boolPtr(true)}, true},
		{"knob false", &PolecatPool{IdleFill: boolPtr(false)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.pool.IdleFillEnabled(); got != c.want {
				t.Errorf("IdleFillEnabled() = %v, want %v", got, c.want)
			}
		})
	}
}

// idle_fill: false has to survive a settings round trip: a *bool field that the
// encoder drops would silently turn the fill back on at the next load.
func TestPolecatPoolIdleFillRoundTrip(t *testing.T) {
	t.Parallel()
	ts := NewTownSettings()
	ts.PolecatPool = &PolecatPool{LocalAgent: "l", MaxLocal: 3, IdleFill: boolPtr(false)}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveTownSettings(path, ts); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"idle_fill": false`) {
		t.Errorf("saved settings must carry idle_fill: false, got %s", raw)
	}
	back, err := LoadOrCreateTownSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.PolecatPool == nil || back.PolecatPool.IdleFillEnabled() {
		t.Errorf("reloaded pool must report the fill off, got %+v", back.PolecatPool)
	}
}
