package rig

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadEffectiveMergeQueueConfig_ThreeTierPrecedence exercises the same
// repo-floor -> rig-root config.json -> rig-local settings/config.json
// layering that gt-me9t fixed for sling_helpers.loadRigCommandVars, applied
// here to the refinery-patrol vars path (gt-hqji: batch config knobs must
// actually reach the formula regardless of which file an operator used).
func TestLoadEffectiveMergeQueueConfig_ThreeTierPrecedence(t *testing.T) {
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown")

	repoRigDir := filepath.Join(rigDir, "mayor", "rig", ".gastown")
	if err := os.MkdirAll(repoRigDir, 0o755); err != nil {
		t.Fatalf("mkdir repo settings dir: %v", err)
	}
	repoSettings := `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "test_command": "repo-floor-test",
    "lint_command": "repo-floor-lint",
    "batch_min_age": "3h"
  }
}`
	if err := os.WriteFile(filepath.Join(repoRigDir, "settings.json"), []byte(repoSettings), 0o644); err != nil {
		t.Fatalf("write repo settings: %v", err)
	}

	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig dir: %v", err)
	}
	rigRootConfig := `{
  "type": "rig",
  "version": 1,
  "name": "gastown",
  "git_url": "https://example.com/gastown.git",
  "default_branch": "main",
  "merge_queue": {
    "test_command": "rig-root-test",
    "batch_enabled": true,
    "batch_max": 8
  }
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigRootConfig), 0o644); err != nil {
		t.Fatalf("write rig root config.json: %v", err)
	}

	localSettingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(localSettingsDir, 0o755); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	localSettings := `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "batch_max": 20
  }
}`
	if err := os.WriteFile(filepath.Join(localSettingsDir, "config.json"), []byte(localSettings), 0o644); err != nil {
		t.Fatalf("write local settings: %v", err)
	}

	mq := LoadEffectiveMergeQueueConfig(townRoot, "gastown")
	if mq == nil {
		t.Fatal("expected non-nil merged config")
	}

	if mq.TestCommand != "rig-root-test" {
		t.Errorf("test_command = %q, want %q (rig-root overrides repo floor)", mq.TestCommand, "rig-root-test")
	}
	if mq.LintCommand != "repo-floor-lint" {
		t.Errorf("lint_command = %q, want %q (repo floor, not overridden elsewhere)", mq.LintCommand, "repo-floor-lint")
	}
	if !mq.IsBatchEnabled() {
		t.Error("batch_enabled should be true (set at rig-root tier)")
	}
	if mq.GetBatchMinAge() != "3h" {
		t.Errorf("batch_min_age = %q, want %q (repo floor, not overridden elsewhere)", mq.GetBatchMinAge(), "3h")
	}
	if mq.GetBatchMax() != 20 {
		t.Errorf("batch_max = %d, want 20 (rig-local settings/config.json wins over rig-root)", mq.GetBatchMax())
	}
}

func TestLoadEffectiveMergeQueueConfig_NoSourcesReturnsNil(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "emptyrig"), 0o755); err != nil {
		t.Fatalf("mkdir rig dir: %v", err)
	}

	mq := LoadEffectiveMergeQueueConfig(townRoot, "emptyrig")
	if mq != nil {
		t.Errorf("expected nil when no merge_queue config exists anywhere, got %+v", mq)
	}
}
