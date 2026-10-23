package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func setupRigNameTestDir(t *testing.T, rigName string, rigConfig *rigConfigLocal, rigsJSON *config.RigsConfig) string {
	t.Helper()
	townRoot := t.TempDir()

	// Create rig directory with config.json
	rigDir := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}

	if rigConfig != nil {
		data, err := json.MarshalIndent(rigConfig, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigDir, "config.json"), data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Create mayor/rigs.json if provided
	if rigsJSON != nil {
		mayorDir := filepath.Join(townRoot, "mayor")
		if err := os.MkdirAll(mayorDir, 0755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(rigsJSON, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	return townRoot
}

func TestRigNameMismatchCheck_AllMatch(t *testing.T) {
	t.Parallel()
	rigCfg := &rigConfigLocal{
		Type:    "rig",
		Version: 1,
		Name:    "myrig",
		Beads:   &rigConfigBeadsLocal{Prefix: "mr"},
	}
	rigsJSON := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"myrig": {
				BeadsConfig: &config.BeadsConfig{Prefix: "mr"},
			},
		},
	}

	townRoot := setupRigNameTestDir(t, "myrig", rigCfg, rigsJSON)

	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "myrig"}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK, got %v: %v", result.Status, result.Details)
	}
}

func TestRigNameMismatchCheck_NameMismatch(t *testing.T) {
	t.Parallel()
	rigCfg := &rigConfigLocal{
		Type:    "rig",
		Version: 1,
		Name:    "oldname",
		Beads:   &rigConfigBeadsLocal{Prefix: "mr"},
	}
	rigsJSON := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"newname": {
				BeadsConfig: &config.BeadsConfig{Prefix: "mr"},
			},
		},
	}

	townRoot := setupRigNameTestDir(t, "newname", rigCfg, rigsJSON)

	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "newname"}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning, got %v", result.Status)
	}
	if len(result.Details) != 1 {
		t.Errorf("expected 1 detail, got %d: %v", len(result.Details), result.Details)
	}
}

func TestRigNameMismatchCheck_PrefixMismatch(t *testing.T) {
	t.Parallel()
	rigCfg := &rigConfigLocal{
		Type:    "rig",
		Version: 1,
		Name:    "myrig",
		Beads:   &rigConfigBeadsLocal{Prefix: "ab"},
	}
	rigsJSON := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"myrig": {
				BeadsConfig: &config.BeadsConfig{Prefix: "xy"},
			},
		},
	}

	townRoot := setupRigNameTestDir(t, "myrig", rigCfg, rigsJSON)

	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "myrig"}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning, got %v", result.Status)
	}
	if len(result.Details) != 1 {
		t.Errorf("expected 1 detail, got %d: %v", len(result.Details), result.Details)
	}
}

func TestRigNameMismatchCheck_BothMismatch(t *testing.T) {
	t.Parallel()
	rigCfg := &rigConfigLocal{
		Type:    "rig",
		Version: 1,
		Name:    "wrongname",
		Beads:   &rigConfigBeadsLocal{Prefix: "ab"},
	}
	rigsJSON := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"myrig": {
				BeadsConfig: &config.BeadsConfig{Prefix: "xy"},
			},
		},
	}

	townRoot := setupRigNameTestDir(t, "myrig", rigCfg, rigsJSON)

	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "myrig"}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning, got %v", result.Status)
	}
	if len(result.Details) != 2 {
		t.Errorf("expected 2 details, got %d: %v", len(result.Details), result.Details)
	}
}

func TestRigNameMismatchCheck_NoConfigJson(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Create rig directory but no config.json
	rigDir := filepath.Join(townRoot, "myrig")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "myrig"}
	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped for missing config.json, got %v", result.Status)
	}
}

func TestRigNameMismatchCheck_Fix(t *testing.T) {
	t.Parallel()
	rigCfg := &rigConfigLocal{
		Type:    "rig",
		Version: 1,
		Name:    "wrongname",
		Beads:   &rigConfigBeadsLocal{Prefix: "ab"},
	}
	rigsJSON := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"myrig": {
				BeadsConfig: &config.BeadsConfig{Prefix: "xy"},
			},
		},
	}

	townRoot := setupRigNameTestDir(t, "myrig", rigCfg, rigsJSON)

	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "myrig"}

	// Verify mismatch is detected
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning before fix, got %v", result.Status)
	}

	// Apply fix
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Re-read config.json and verify both fields are corrected
	fixed, err := loadRigConfigLocal(filepath.Join(townRoot, "myrig"))
	if err != nil {
		t.Fatalf("failed to load fixed config: %v", err)
	}

	if fixed.Name != "myrig" {
		t.Errorf("expected Name=%q after fix, got %q", "myrig", fixed.Name)
	}
	if fixed.Beads == nil || fixed.Beads.Prefix != "xy" {
		prefix := ""
		if fixed.Beads != nil {
			prefix = fixed.Beads.Prefix
		}
		t.Errorf("expected Beads.Prefix=%q after fix, got %q", "xy", prefix)
	}

	// Re-run check to confirm clean
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %v", result.Status, result.Details)
	}
}

// TestRigNameMismatchFixPreservesFields pins the fix to the one value it
// corrects: a renamed rig's config.json keeps its merge queue, pool settings
// and URLs (gt-ay3b0).
func TestRigNameMismatchFixPreservesFields(t *testing.T) {
	t.Parallel()
	const configJSON = `{
  "type": "rig",
  "version": 1,
  "name": "oldname",
  "git_url": "https://example.com/repo.git",
  "push_url": "git@example.com:me/repo.git",
  "upstream_url": "https://example.com/upstream.git",
  "local_repo": "/home/me/repo",
  "created_at": "2025-01-01T00:00:00Z",
  "default_branch": "main",
  "beads": {"prefix": "mr", "repo": "local"},
  "merge_queue": {
    "build_command": "make build",
    "test_command": "make test",
    "editorial": {"required": true}
  },
  "witness": {"retired_field": "kept verbatim"},
  "polecat_pool_size": 3,
  "polecat_names": ["alpha", "beta"]
}`
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "myrig")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "myrig"}
	if result := check.Run(ctx); result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning before fix, got %v: %v", result.Status, result.Details)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	fixedData, err := os.ReadFile(filepath.Join(rigDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(configJSON), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixedData, &after); err != nil {
		t.Fatalf("fixed config.json does not parse: %v\n%s", err, fixedData)
	}

	if after["name"] != "myrig" {
		t.Errorf("expected name corrected to %q, got %v", "myrig", after["name"])
	}
	for key, want := range before {
		if key == "name" {
			continue
		}
		got, ok := after[key]
		if !ok {
			t.Errorf("fix deleted key %q", key)
			continue
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("fix changed key %q: want %v, got %v", key, want, got)
		}
	}
	if _, ok := after["merge_queue"].(map[string]any); !ok {
		t.Errorf("merge_queue is not an object after fix: %v", after["merge_queue"])
	}
}

// TestRigNameMismatchFixKeepsBeadsSiblingKeys covers the beads.prefix rewrite
// with a sibling key inside beads: the prefix changes, beads.repo does not.
func TestRigNameMismatchFixKeepsBeadsSiblingKeys(t *testing.T) {
	t.Parallel()
	const configJSON = `{
  "type": "rig",
  "version": 1,
  "name": "myrig",
  "beads": {"prefix": "mr", "repo": "local"},
  "merge_queue": {"test_command": "make test"}
}`
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "myrig")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}
	rigsJSON := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"myrig": {BeadsConfig: &config.BeadsConfig{Prefix: "xy"}},
		},
	}
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	rigsData, err := json.MarshalIndent(rigsJSON, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), rigsData, 0644); err != nil {
		t.Fatal(err)
	}

	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "myrig"}
	if result := check.Run(ctx); result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning before fix, got %v: %v", result.Status, result.Details)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	fixed, err := loadRigConfigRaw(rigDir)
	if err != nil {
		t.Fatalf("fixed config.json does not parse: %v", err)
	}
	var beads map[string]string
	if err := json.Unmarshal(fixed["beads"], &beads); err != nil {
		t.Fatalf("beads does not parse after fix: %v", err)
	}
	if beads["prefix"] != "xy" {
		t.Errorf("expected beads.prefix %q after fix, got %q", "xy", beads["prefix"])
	}
	if beads["repo"] != "local" {
		t.Errorf("fix changed beads.repo: want %q, got %q", "local", beads["repo"])
	}
	if _, ok := fixed["merge_queue"]; !ok {
		t.Errorf("fix deleted merge_queue")
	}
	if result := check.Run(ctx); result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %v", result.Status, result.Details)
	}
}

// TestRigNameMismatchFixRejectsNonObjectConfig pins that a config.json which
// parses as JSON null is reported as an error rather than written to.
func TestRigNameMismatchFixRejectsNonObjectConfig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "myrig")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	const original = "null"
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "myrig"}
	if err := check.Fix(ctx); err == nil {
		t.Fatal("expected Fix to return an error for a non-object config.json")
	}

	data, err := os.ReadFile(filepath.Join(rigDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Errorf("Fix rewrote a non-object config.json: got %q", data)
	}
}

func TestRigNameMismatchCheck_NoRig(t *testing.T) {
	t.Parallel()
	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: t.TempDir(), RigName: ""}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no rig specified, got %v", result.Status)
	}
}

func TestRigNameMismatchCheck_NoRigsJson(t *testing.T) {
	t.Parallel()
	// Config name matches directory, but no rigs.json — should only check name
	rigCfg := &rigConfigLocal{
		Type:    "rig",
		Version: 1,
		Name:    "myrig",
		Beads:   &rigConfigBeadsLocal{Prefix: "mr"},
	}

	townRoot := setupRigNameTestDir(t, "myrig", rigCfg, nil)

	check := NewRigNameMismatchCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "myrig"}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when name matches and no rigs.json, got %v: %v", result.Status, result.Details)
	}
}
