package rig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/wisp"
)

func TestGetConfig_SystemDefaults(t *testing.T) {
	t.Parallel()
	// Create a temp rig with no wisp or bead config
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{
		Name:          "testrig",
		Path:          rigPath,
		IdentityBeads: noRigBead(),
	}

	// Should get system defaults
	result := rig.GetConfigWithSource("default_formula")
	if result.Source != SourceSystem {
		t.Errorf("expected source SourceSystem, got %s", result.Source)
	}
	if result.Value != "mol-polecat-work" {
		t.Errorf("expected value 'mol-polecat-work', got %v", result.Value)
	}

	// Test boolean default
	if !rig.GetBoolConfig("auto_restart") {
		t.Error("expected auto_restart to be true by default")
	}

	// Test int default. max_polecats defaults to 0, which is "no per-rig
	// concurrency cap": an unset rig must not be throttled by the compiled-in
	// default (gt-1kbi). The per-rig directory cap floors at
	// minPolecatDirsPerRig either way, so nothing else about the default moves.
	maxPolecats := rig.GetIntConfig("max_polecats")
	if maxPolecats != 0 {
		t.Errorf("expected max_polecats=0 (uncapped), got %d", maxPolecats)
	}
}

func TestGetConfig_WispOverride(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{
		Name:          "testrig",
		Path:          rigPath,
		IdentityBeads: noRigBead(),
	}

	// Create wisp config with override
	wispCfg := wisp.NewConfig(tmpDir, "testrig")
	if err := wispCfg.Set("default_formula", "mol-other"); err != nil {
		t.Fatal(err)
	}

	// Should get wisp value
	result := rig.GetConfigWithSource("default_formula")
	if result.Source != SourceWisp {
		t.Errorf("expected source SourceWisp, got %s", result.Source)
	}
	if result.Value != "mol-other" {
		t.Errorf("expected value 'mol-other', got %v", result.Value)
	}
}

func TestGetConfig_WispBlocked(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{
		Name:          "testrig",
		Path:          rigPath,
		IdentityBeads: noRigBead(),
	}

	// Block auto_restart at wisp layer
	wispCfg := wisp.NewConfig(tmpDir, "testrig")
	if err := wispCfg.Block("auto_restart"); err != nil {
		t.Fatal(err)
	}

	// Should return nil (blocked)
	result := rig.GetConfigWithSource("auto_restart")
	if result.Source != SourceBlocked {
		t.Errorf("expected source SourceBlocked, got %s", result.Source)
	}
	if result.Value != nil {
		t.Errorf("expected nil value for blocked key, got %v", result.Value)
	}

	// Bool getter should return false for blocked
	if rig.GetBoolConfig("auto_restart") {
		t.Error("expected auto_restart to be false when blocked")
	}
}

func TestGetIntConfig_Stacking(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{
		Name:          "testrig",
		Path:          rigPath,
		IdentityBeads: noRigBead(),
	}

	// Set wisp adjustment
	wispCfg := wisp.NewConfig(tmpDir, "testrig")
	if err := wispCfg.Set("priority_adjustment", 5); err != nil {
		t.Fatal(err)
	}

	// priority_adjustment uses stacking: base (0) + wisp (5) = 5
	result := rig.GetIntConfig("priority_adjustment")
	if result != 5 {
		t.Errorf("expected priority_adjustment=5, got %d", result)
	}
}

func TestGetBoolConfig_StringConversion(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{
		Name:          "testrig",
		Path:          rigPath,
		IdentityBeads: noRigBead(),
	}

	// Set string "true" in wisp
	wispCfg := wisp.NewConfig(tmpDir, "testrig")
	if err := wispCfg.Set("custom_bool", "true"); err != nil {
		t.Fatal(err)
	}

	if !rig.GetBoolConfig("custom_bool") {
		t.Error("expected 'true' string to convert to bool true")
	}

	// Set string "false"
	if err := wispCfg.Set("custom_bool", "false"); err != nil {
		t.Fatal(err)
	}

	if rig.GetBoolConfig("custom_bool") {
		t.Error("expected 'false' string to convert to bool false")
	}
}

func TestGetConfig_UnknownKey(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{
		Name:          "testrig",
		Path:          rigPath,
		IdentityBeads: noRigBead(),
	}

	result := rig.GetConfigWithSource("nonexistent_key")
	if result.Source != SourceNone {
		t.Errorf("expected source SourceNone, got %s", result.Source)
	}
	if result.Value != nil {
		t.Errorf("expected nil value for unknown key, got %v", result.Value)
	}
}

func TestGetStringConfig(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{
		Name:          "testrig",
		Path:          rigPath,
		IdentityBeads: noRigBead(),
	}

	// System default for default_formula
	formula := rig.GetStringConfig("default_formula")
	if formula != "mol-polecat-work" {
		t.Errorf("expected default_formula='mol-polecat-work', got %s", formula)
	}

	// Unknown key
	unknown := rig.GetStringConfig("nonexistent")
	if unknown != "" {
		t.Errorf("expected empty string for unknown key, got %s", unknown)
	}
}

func TestCoerceInt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    interface{}
		expected int
	}{
		{nil, 0},
		{0, 0},
		{42, 42},
		{int64(100), 100},
		{float64(3.14), 3},
		{"123", 123},
		{"abc", 0},
		{false, 0},
		{true, 1}, // "1" written to an int key as bool before keys were typed
	}

	for _, tc := range tests {
		result := CoerceInt(tc.input)
		if result != tc.expected {
			t.Errorf("CoerceInt(%v) = %d, expected %d", tc.input, result, tc.expected)
		}
	}
}

func TestCoerceBool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    interface{}
		expected bool
	}{
		{nil, false}, // unset
		{false, false},
		{true, true},
		{"true", true},
		{"false", false},
		{"TRUE", true}, // bead labels are written verbatim
		{"yes", true},  // legacy spelling
		{"on", true},   // as accepted by `gt config set` and `gt rig config set`
		{"OFF", false},
		{"1", true},
		{"0", false},
		{1, true},
		{0, false},
		{int64(1), true},
		{float64(1), true}, // wisp values load from JSON as float64
		{float64(0), false},
		{"banana", false}, // unrecognized reads as false
	}

	for _, tc := range tests {
		if got := CoerceBool(tc.input); got != tc.expected {
			t.Errorf("CoerceBool(%v) = %v, expected %v", tc.input, got, tc.expected)
		}
	}
}

// TestGetIntConfig_LegacyBoolValue covers rigs whose max_polecats was written as
// a boolean by an older `gt rig config set` ("1" was parsed as true before values
// were typed by key). The cap must still read as the 1 the operator asked for,
// not as 0 with a bool silently ignored.
func TestGetIntConfig_LegacyBoolValue(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{Name: "testrig", Path: rigPath, IdentityBeads: noRigBead()}

	wispCfg := wisp.NewConfig(tmpDir, "testrig")
	if err := wispCfg.Set("max_polecats", true); err != nil {
		t.Fatalf("seed wisp config: %v", err)
	}

	if got := rig.GetIntConfig("max_polecats"); got != 1 {
		t.Errorf("max_polecats = %d, expected 1 for a legacy bool value", got)
	}
}

// TestGetBoolConfig_NumericValue covers auto_restart written as a number. A raw
// `val.(bool)` assertion (what the daemon used to do) ignores the value entirely,
// so a stored 0 silently leaves auto-restart enabled.
func TestGetBoolConfig_NumericValue(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{Name: "testrig", Path: rigPath, IdentityBeads: noRigBead()}

	wispCfg := wisp.NewConfig(tmpDir, "testrig")
	if err := wispCfg.Set("auto_restart", float64(0)); err != nil {
		t.Fatalf("seed wisp config: %v", err)
	}

	if rig.GetBoolConfig("auto_restart") {
		t.Error("auto_restart=0 should read as false, not fall through to the default")
	}
}

// TestGetConfig_BeadLabelThroughIdentityBeads pins the rig identity bead
// layer: the label on the rig identity bead wins over the system default.
func TestGetConfig_BeadLabelThroughIdentityBeads(t *testing.T) {
	t.Parallel()
	rigPath := filepath.Join(t.TempDir(), "testrig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-rig-testrig", Title: "testrig", Labels: []string{"polecat_branch_template:team/{name}"}})
	r := &Rig{Name: "testrig", Path: rigPath, IdentityBeads: db}
	result := r.GetConfigWithSource("polecat_branch_template")
	if result.Source != SourceBead || result.Value != "team/{name}" {
		t.Fatalf("GetConfigWithSource = %+v, want the bead label team/{name}", result)
	}
}

// noRigBead is a database with no rig identity bead: the bead layer has
// nothing, so lookups fall through to the system defaults.
func noRigBead() beads.Client { return beadsfake.New() }
