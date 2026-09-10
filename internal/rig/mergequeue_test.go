package rig

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveMergeQueueConfig_Precedence mirrors
// TestLoadRigCommandVarsPrecedence in internal/cmd/sling_helpers_test.go: rig
// root config.json is the floor, the repo-committed .gastown/settings.json
// overrides it, and rig-local settings/config.json has the final say. Both
// gt sling (via loadRigCommandVars) and gt done (via resolvePreVerifiedClaim)
// call ResolveMergeQueueConfig, so this test guards the single resolver both
// depend on (gt-k4sy).
func TestResolveMergeQueueConfig_Precedence(t *testing.T) {
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown")
	repoRoot := filepath.Join(rigDir, "mayor", "rig")
	gastownDir := filepath.Join(repoRoot, ".gastown")
	settingsDir := filepath.Join(rigDir, "settings")
	for _, dir := range []string{gastownDir, settingsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "gastown",
  "git_url": "https://github.com/sloanahrens/gastown.git",
  "default_branch": "main",
  "beads": {"prefix": "gt"},
  "merge_queue": {
    "build_command": "make build-root",
    "test_command": "make test-root",
    "lint_command": "make lint-root"
  }
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}

	repoSettings := `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "build_command": "make build-repo",
    "test_command": "make test-repo"
  }
}`
	if err := os.WriteFile(filepath.Join(gastownDir, "settings.json"), []byte(repoSettings), 0o644); err != nil {
		t.Fatalf("write repo settings.json: %v", err)
	}

	localSettings := `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "build_command": "make build-local"
  }
}`
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(localSettings), 0o644); err != nil {
		t.Fatalf("write settings/config.json: %v", err)
	}

	mq := ResolveMergeQueueConfig(townRoot, "gastown")
	if mq == nil {
		t.Fatal("ResolveMergeQueueConfig() = nil, want non-nil")
	}
	if mq.BuildCommand != "make build-local" {
		t.Errorf("BuildCommand = %q, want %q (rig-local wins)", mq.BuildCommand, "make build-local")
	}
	if mq.TestCommand != "make test-repo" {
		t.Errorf("TestCommand = %q, want %q (repo wins over root floor)", mq.TestCommand, "make test-repo")
	}
	if mq.LintCommand != "make lint-root" {
		t.Errorf("LintCommand = %q, want %q (falls through to root floor)", mq.LintCommand, "make lint-root")
	}
}

// TestResolveMergeQueueConfig_NoConfig verifies the nil-townRoot/nil-rigName
// and no-config-anywhere cases return nil, matching HasAnyGateCommand's
// nil-safety.
func TestResolveMergeQueueConfig_NoConfig(t *testing.T) {
	if got := ResolveMergeQueueConfig("", "gastown"); got != nil {
		t.Errorf("ResolveMergeQueueConfig(empty townRoot) = %+v, want nil", got)
	}
	if got := ResolveMergeQueueConfig("/tmp", ""); got != nil {
		t.Errorf("ResolveMergeQueueConfig(empty rigName) = %+v, want nil", got)
	}

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown"), 0o755); err != nil {
		t.Fatalf("mkdir rig dir: %v", err)
	}
	if got := ResolveMergeQueueConfig(townRoot, "gastown"); got != nil {
		t.Errorf("ResolveMergeQueueConfig() with no config files = %+v, want nil", got)
	}
}
