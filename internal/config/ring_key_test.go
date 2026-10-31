package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ringLiteral is shaped like a signing key. No error here may carry it.
const ringLiteral = "literal-ring-signing-key-value"

func TestRingKeyMustBeAReference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key string
		ok  bool
	}{
		{"${GT_RING_KEY}", true},
		{ringLiteral, false},
		{"${GT_RING_KEY:-fallback}", false},
		{"prefix-${GT_RING_KEY}", false},
		{"${GT_RING_KEY}${OTHER}", false},
		{"${1BAD}", false},
		{"", false},
	}
	for _, tc := range cases {
		err := ValidateRingKey(tc.key)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateRingKey(%q) = %v, want ok=%v", tc.key, err, tc.ok)
		}
		if err != nil && tc.key != "" && strings.Contains(err.Error(), tc.key) {
			t.Errorf("ValidateRingKey(%q) error echoes the value: %v", tc.key, err)
		}
	}
}

func TestRingKeyResolvesThroughTheEnvironment(t *testing.T) {
	t.Parallel()
	settings := &TownSettings{Ring: &RingConfig{Key: "${GT_RING_KEY}"}}
	env := map[string]string{"GT_RING_KEY": ringLiteral}
	got, err := settings.RingKeyFrom(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("RingKeyFrom: %v", err)
	}
	if got != ringLiteral {
		t.Fatalf("RingKeyFrom = %q, want the variable's value", got)
	}
}

func TestRingKeyRefusesWhenNotConfigured(t *testing.T) {
	t.Parallel()
	cases := map[string]*TownSettings{
		"no settings":    nil,
		"no ring block":  {},
		"empty ring key": {Ring: &RingConfig{}},
	}
	for name, settings := range cases {
		_, err := settings.RingKeyFrom(func(string) string { return ringLiteral })
		if err == nil {
			t.Errorf("%s: RingKeyFrom = nil error, want a refusal", name)
		}
	}
}

// TestRingKeyRefusesALiteralOnResolve: a literal key that reaches resolution
// without passing through a settings load is refused there too, and the
// refusal omits the value.
func TestRingKeyRefusesALiteralOnResolve(t *testing.T) {
	t.Parallel()
	settings := &TownSettings{Ring: &RingConfig{Key: ringLiteral}}
	_, err := settings.RingKeyFrom(func(string) string { return ringLiteral })
	if err == nil {
		t.Fatal("RingKeyFrom of a literal = nil error, want a refusal")
	}
	if strings.Contains(err.Error(), ringLiteral) {
		t.Fatal("RingKeyFrom's refusal echoes the literal")
	}
}

// TestRingKeyRefusesAnUnsetVariableByName: a reference to a variable the
// daemon environment does not set names the variable, never a value.
func TestRingKeyRefusesAnUnsetVariableByName(t *testing.T) {
	t.Parallel()
	settings := &TownSettings{Ring: &RingConfig{Key: "${GT_RING_KEY}"}}
	key, err := settings.RingKeyFrom(func(string) string { return "" })
	if err == nil {
		t.Fatalf("RingKeyFrom with an unset variable = %q, want a refusal", key)
	}
	if !strings.Contains(err.Error(), "GT_RING_KEY") {
		t.Fatalf("error %q does not name the variable", err)
	}
}

// TestRingKeyLiteralInSettingsFileLoadsButVerbsRefuse: a literal ring.key does
// not fail a settings load, so one bad field cannot break every reader of the
// town. The Ring verb that resolves the key refuses the literal, and its error
// omits the value.
func TestRingKeyLiteralInSettingsFileLoadsButVerbsRefuse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := TownSettingsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(&TownSettings{Type: "town-settings", Version: CurrentTownSettingsVersion, Ring: &RingConfig{Key: ringLiteral}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaders := map[string]func(string) (*TownSettings, error){
		"LoadTownSettings":         LoadTownSettings,
		"LoadOrCreateTownSettings": LoadOrCreateTownSettings,
	}
	for name, load := range loaders {
		settings, err := load(path)
		if err != nil {
			t.Errorf("%s refused the file over a literal ring.key: %v", name, err)
			continue
		}
		_, err = settings.RingKeyFrom(func(string) string { return ringLiteral })
		if err == nil {
			t.Errorf("%s: RingKeyFrom resolved a literal ring.key", name)
			continue
		}
		if strings.Contains(err.Error(), ringLiteral) {
			t.Errorf("%s: RingKeyFrom error echoes the literal: %v", name, err)
		}
	}
}

// TestRingKeyLoadsATownWithNoRingKey: a town that never named a ring key still
// loads; only the Ring verbs refuse, when they need the key.
func TestRingKeyLoadsATownWithNoRingKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := TownSettingsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"town-settings","version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTownSettings(path); err != nil {
		t.Fatalf("LoadTownSettings of a town with no ring key: %v", err)
	}
}

// TestRingKeyLoadsAReferenceFromSettingsFile: the reference form loads and
// resolves, so the refusal above is the literal's alone.
func TestRingKeyLoadsAReferenceFromSettingsFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := TownSettingsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(&TownSettings{Type: "town-settings", Version: CurrentTownSettingsVersion, Ring: &RingConfig{Key: "${GT_RING_KEY}"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadTownSettings(path)
	if err != nil {
		t.Fatalf("LoadTownSettings: %v", err)
	}
	if _, err := settings.RingKeyFrom(func(string) string { return ringLiteral }); err != nil {
		t.Fatalf("RingKeyFrom after a reference load: %v", err)
	}
}
