package rig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// writeRigConfigFile writes body to <rigPath>/config.json, creating rigPath.
func writeRigConfigFile(t *testing.T, rigPath, body string) {
	t.Helper()
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rigPath, err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

const typoRigConfigJSON = `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "default_branch": "develop",
  "merge_queue": {"build_command": "make build"},
  "polecat_names": ["alpha", "beta"],
  "polecat_namess": ["alpha", "beta"]
}`

// TestLoadRigConfigIfPresent_AbsentConfig pins the distinction the accessor
// exists for (gt-w8dw5): a rig with no config.json is not an error, so callers
// keep falling back without a warning.
func TestLoadRigConfigIfPresent_AbsentConfig(t *testing.T) {
	t.Parallel()

	cfg, err := LoadRigConfigIfPresent(t.TempDir())
	if err != nil {
		t.Fatalf("LoadRigConfigIfPresent(no config.json) error = %v; want nil", err)
	}
	if cfg != nil {
		t.Errorf("LoadRigConfigIfPresent(no config.json) = %+v; want nil", cfg)
	}
}

// TestLoadRigConfigIfPresent_UnknownKey pins the other half: a config.json an
// operator typo'd reports the parse error rather than reading as absent.
func TestLoadRigConfigIfPresent_UnknownKey(t *testing.T) {
	t.Parallel()

	rigPath := t.TempDir()
	writeRigConfigFile(t, rigPath, typoRigConfigJSON)

	cfg, err := LoadRigConfigIfPresent(rigPath)
	if err == nil {
		t.Fatalf("LoadRigConfigIfPresent(typo'd key) = %+v, nil; want an error", cfg)
	}
	if !errors.Is(err, config.ErrUnparseable) {
		t.Errorf("error = %v; want it to wrap config.ErrUnparseable", err)
	}
}

// TestDefaultBranch_ValidConfigUnchanged is the positive half: a config.json
// that decodes still supplies default_branch, and nothing is reported.
func TestDefaultBranch_ValidConfigUnchanged(t *testing.T) {
	t.Parallel()

	rigPath := t.TempDir()
	writeRigConfigFile(t, rigPath, `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "default_branch": "develop"
}`)

	r := &Rig{Name: "testrig", Path: rigPath}
	if got := r.DefaultBranch(); got != "develop" {
		t.Errorf("DefaultBranch() = %q; want %q", got, "develop")
	}
	if RigConfigWarned(rigPath) {
		t.Errorf("RigConfigWarned(%s) = true for a valid config", rigPath)
	}
}

// TestDefaultBranch_UnparseableConfigReports pins the fix: the typo no longer
// yields a silent "main" with the operator's file looking authoritative.
func TestDefaultBranch_UnparseableConfigReports(t *testing.T) {
	t.Parallel()

	rigPath := t.TempDir()
	writeRigConfigFile(t, rigPath, typoRigConfigJSON)

	r := &Rig{Name: "testrig", Path: rigPath}
	if got := r.DefaultBranch(); got != "main" {
		t.Errorf("DefaultBranch() = %q; want the main fallback", got)
	}
	if !RigConfigWarned(rigPath) {
		t.Errorf("DefaultBranch() fell back without reporting the parse error")
	}
}

// TestResolveMergeQueueConfig_ValidConfigUnchanged is the positive half for the
// merge-queue floor.
func TestResolveMergeQueueConfig_ValidConfigUnchanged(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	writeRigConfigFile(t, filepath.Join(townRoot, "testrig"), `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "merge_queue": {"build_command": "make build"}
}`)

	mq := ResolveMergeQueueConfig(townRoot, "testrig")
	if mq == nil {
		t.Fatalf("ResolveMergeQueueConfig() = nil; want the rig-root merge_queue floor")
	}
	if mq.BuildCommand != "make build" {
		t.Errorf("BuildCommand = %q; want %q", mq.BuildCommand, "make build")
	}
}

// TestResolveMergeQueueConfig_UnparseableConfigReports pins that a dropped
// floor is reported rather than silently resolved from repo/local settings.
func TestResolveMergeQueueConfig_UnparseableConfigReports(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	writeRigConfigFile(t, rigPath, typoRigConfigJSON)

	mq := ResolveMergeQueueConfig(townRoot, "testrig")
	if !RigConfigWarned(rigPath) {
		t.Errorf("ResolveMergeQueueConfig() dropped the rig-root floor without reporting the parse error (returned %+v)", mq)
	}
}
