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

// --- Default*Config functions ---

func TestDefaultFeedCuratorConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultFeedCuratorConfig()

	if cfg == nil {
		t.Fatal("DefaultFeedCuratorConfig() returned nil")
	}

	dedupe := ParseDurationOrDefault(cfg.DoneDedupeWindow, 0)
	if dedupe != 10*time.Second {
		t.Errorf("DoneDedupeWindow = %v, want 10s", dedupe)
	}
	agg := ParseDurationOrDefault(cfg.SlingAggregateWindow, 0)
	if agg != 30*time.Second {
		t.Errorf("SlingAggregateWindow = %v, want 30s", agg)
	}
	if cfg.MinAggregateCount != 3 {
		t.Errorf("MinAggregateCount = %d, want 3", cfg.MinAggregateCount)
	}
}

// --- JSON serialization round-trips ---

func TestFeedCuratorConfig_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	original := &FeedCuratorConfig{
		DoneDedupeWindow:     "20s",
		SlingAggregateWindow: "1m",
		MinAggregateCount:    5,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var loaded FeedCuratorConfig
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if loaded != *original {
		t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", loaded, *original)
	}
}

// --- TownSettings with/without new config fields ---

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

	// Fields absent from the JSON should be nil (omitempty → nil pointer)
	if ts.FeedCurator != nil {
		t.Errorf("FeedCurator should be nil for legacy config, got %+v", ts.FeedCurator)
	}

	// Existing fields should still load correctly
	if ts.DefaultAgent != "claude" {
		t.Errorf("DefaultAgent = %q, want %q", ts.DefaultAgent, "claude")
	}
}

func TestTownSettings_WithNewFields_RoundTrip(t *testing.T) {
	t.Parallel()
	// Save TownSettings WITH all new config fields, then reload and verify.
	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, "config.json")

	original := NewTownSettings()
	original.FeedCurator = &FeedCuratorConfig{
		DoneDedupeWindow:     "20s",
		SlingAggregateWindow: "1m",
		MinAggregateCount:    5,
	}

	if err := SaveTownSettings(settingsPath, original); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}

	loaded, err := LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}

	// Verify FeedCurator
	if loaded.FeedCurator == nil {
		t.Fatal("FeedCurator is nil after round-trip")
	}
	if loaded.FeedCurator.DoneDedupeWindow != "20s" {
		t.Errorf("DoneDedupeWindow = %q, want %q", loaded.FeedCurator.DoneDedupeWindow, "20s")
	}
	if loaded.FeedCurator.SlingAggregateWindow != "1m" {
		t.Errorf("SlingAggregateWindow = %q, want %q", loaded.FeedCurator.SlingAggregateWindow, "1m")
	}
	if loaded.FeedCurator.MinAggregateCount != 5 {
		t.Errorf("MinAggregateCount = %d, want %d", loaded.FeedCurator.MinAggregateCount, 5)
	}
}

func TestTownSettings_PartialNewFields(t *testing.T) {
	t.Parallel()
	// Only some fields are set; the rest should remain nil.
	settingsJSON := `{
		"type": "town-settings",
		"version": 1,
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

	// FeedCurator present with partial fields
	if ts.FeedCurator == nil {
		t.Fatal("FeedCurator should not be nil")
	}
	if ts.FeedCurator.DoneDedupeWindow != "25s" {
		t.Errorf("DoneDedupeWindow = %q, want %q", ts.FeedCurator.DoneDedupeWindow, "25s")
	}
	// Unset fields within the struct should be zero-value (empty string)
	if ts.FeedCurator.SlingAggregateWindow != "" {
		t.Errorf("SlingAggregateWindow = %q, want empty", ts.FeedCurator.SlingAggregateWindow)
	}
	// ParseDurationOrDefault should apply fallback for empty fields
	agg := ParseDurationOrDefault(ts.FeedCurator.SlingAggregateWindow, 30*time.Second)
	if agg != 30*time.Second {
		t.Errorf("ParseDurationOrDefault for empty SlingAggregateWindow = %v, want 30s", agg)
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
	// New config sections should be nil (NewTownSettings doesn't set them)
	if ts.FeedCurator != nil {
		t.Errorf("FeedCurator should be nil for defaults")
	}
}

// --- omitempty behavior: nil config fields must not appear in JSON ---

func TestTownSettings_OmitemptyNilFields(t *testing.T) {
	t.Parallel()
	ts := NewTownSettings()

	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	jsonStr := string(data)
	for _, key := range []string{"feed_curator"} {
		if strings.Contains(jsonStr, key) {
			t.Errorf("JSON should not contain %q when field is nil, got:\n%s", key, jsonStr)
		}
	}
}

func TestTownSettings_OmitemptyEmptyDurations(t *testing.T) {
	t.Parallel()
	// When config struct is set but all duration fields are empty,
	// omitempty on the string fields means they should be absent from JSON.
	ts := NewTownSettings()
	ts.FeedCurator = &FeedCuratorConfig{} // all zero values

	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	jsonStr := string(data)
	// The "feed_curator" key SHOULD appear (pointer is non-nil)
	if !strings.Contains(jsonStr, "feed_curator") {
		t.Error("JSON should contain feed_curator when struct is non-nil")
	}
	// But individual empty string fields should be omitted
	for _, key := range []string{"done_dedupe_window", "sling_aggregate_window"} {
		if strings.Contains(jsonStr, key) {
			t.Errorf("JSON should not contain %q when field is empty string, got:\n%s", key, jsonStr)
		}
	}
}

func TestFeedCuratorConfig_OmitemptyZeroCount(t *testing.T) {
	t.Parallel()
	cfg := &FeedCuratorConfig{
		DoneDedupeWindow: "10s",
		// MinAggregateCount=0 should be omitted (omitempty on int)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	jsonStr := string(data)
	if strings.Contains(jsonStr, "min_aggregate_count") {
		t.Errorf("JSON should not contain min_aggregate_count when 0, got: %s", jsonStr)
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
