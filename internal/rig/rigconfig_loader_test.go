package rig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestLoadRigConfig_StrictUnknownKey covers the merge of rig.RigConfig into
// config.RigConfig (gt-y3pgh.2.5): rig.LoadRigConfig now delegates to the
// strict config.LoadRigConfig, so a <rig>/config.json an operator typo'd
// fails closed instead of loading with the knob silently dropped — which is
// how the old json.Unmarshal behavior taught operators to distrust config.
func TestLoadRigConfig_StrictUnknownKey(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	body := `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "git_url": "https://example.invalid/testrig.git",
  "default_brach": "main"
}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	cfg, err := LoadRigConfig(rigPath)
	if err == nil {
		t.Fatalf("LoadRigConfig() = %+v, nil; want an error for the unknown key", cfg)
	}
	if !errors.Is(err, config.ErrUnparseable) {
		t.Errorf("LoadRigConfig() error = %v; want it to wrap config.ErrUnparseable", err)
	}
}

// TestLoadRigConfig_FieldsTheRigManagerWrites is the positive half: the exact
// keys internal/rig writes into <rig>/config.json — default_branch,
// merge_queue and polecat_names — must decode through the one loader. These
// are the fields that motivated the alias: config.RigConfig had to declare
// them for strict decoding to accept the file the rig manager writes.
func TestLoadRigConfig_FieldsTheRigManagerWrites(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	body := `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "git_url": "https://example.invalid/testrig.git",
  "created_at": "2026-01-02T03:04:05Z",
  "default_branch": "develop",
  "beads": {"prefix": "tr"},
  "merge_queue": {"build_command": "make build", "test_command": "make test"},
  "polecat_pool_size": 3,
  "polecat_names": ["alpha", "beta", "gamma"]
}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	cfg, err := LoadRigConfig(rigPath)
	if err != nil {
		t.Fatalf("LoadRigConfig() error = %v", err)
	}
	if cfg.DefaultBranch != "develop" {
		t.Errorf("DefaultBranch = %q, want %q", cfg.DefaultBranch, "develop")
	}
	if cfg.MergeQueue == nil || cfg.MergeQueue.BuildCommand != "make build" {
		t.Errorf("MergeQueue = %+v, want build_command %q", cfg.MergeQueue, "make build")
	}
	if len(cfg.PolecatNames) != 3 || cfg.PolecatNames[0] != "alpha" {
		t.Errorf("PolecatNames = %v, want [alpha beta gamma]", cfg.PolecatNames)
	}
	if cfg.Beads == nil || cfg.Beads.Prefix != "tr" {
		t.Errorf("Beads = %+v, want prefix %q", cfg.Beads, "tr")
	}
}

// TestRigConfigIsConfigRigConfig pins the alias: callers that name
// rig.RigConfig and callers that name config.RigConfig must be the same type,
// so neither has to convert and the two cannot drift apart again.
func TestRigConfigIsConfigRigConfig(t *testing.T) {
	t.Parallel()
	var fromRig *RigConfig
	fromConfig := &config.RigConfig{Name: "testrig", DefaultBranch: "main"}
	fromRig = fromConfig
	if fromRig.Name != "testrig" {
		t.Errorf("rig.RigConfig and config.RigConfig are not the same type")
	}

	var beads *BeadsConfig = &config.BeadsConfig{Prefix: "tr"}
	if beads.Prefix != "tr" {
		t.Errorf("rig.BeadsConfig and config.BeadsConfig are not the same type")
	}
}

// TestSaveRigConfigRoundTripsThroughStrictWriter guards the write half: the
// rig manager's save goes through config.SaveRigConfig, so a config it wrote
// must decode strictly back through config.LoadRigConfig (the writer refuses
// to replace a file it cannot decode, so a shape it produces must survive).
func TestSaveRigConfigRoundTripsThroughStrictWriter(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	cfg := &RigConfig{
		Type:          "rig",
		Version:       CurrentRigConfigVersion,
		Name:          "testrig",
		GitURL:        "https://example.invalid/testrig.git",
		DefaultBranch: "main",
		Beads:         &BeadsConfig{Prefix: "tr"},
	}
	if err := config.SaveRigConfig(filepath.Join(rigPath, "config.json"), cfg); err != nil {
		t.Fatalf("SaveRigConfig() error = %v", err)
	}

	got, err := LoadRigConfig(rigPath)
	if err != nil {
		t.Fatalf("LoadRigConfig() error = %v", err)
	}
	if got.Name != "testrig" || got.DefaultBranch != "main" || got.Beads.Prefix != "tr" {
		t.Errorf("round trip = %+v, want name testrig, default_branch main, prefix tr", got)
	}
}
