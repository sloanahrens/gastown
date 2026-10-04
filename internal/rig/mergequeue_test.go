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
	t.Parallel()
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

// TestResolveMergeQueueConfig_RigRootAbsent is the negative case flagged in
// gt-egiv finding 2: when the rig root has no merge_queue section at all
// (the pre-gt-me9t shape), the repo/local merge must behave exactly as it
// did before the rig-root floor was introduced — proving the floor is
// additive, not a silent precedence inversion.
func TestResolveMergeQueueConfig_RigRootAbsent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown")
	repoRoot := filepath.Join(rigDir, "mayor", "rig")
	gastownDir := filepath.Join(repoRoot, ".gastown")
	if err := os.MkdirAll(gastownDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", gastownDir, err)
	}

	// Rig root config.json exists but has no merge_queue key at all.
	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "gastown",
  "git_url": "https://github.com/sloanahrens/gastown.git"
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}

	repoSettings := `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {"test_command": "make test-repo"}
}`
	if err := os.WriteFile(filepath.Join(gastownDir, "settings.json"), []byte(repoSettings), 0o644); err != nil {
		t.Fatalf("write repo settings.json: %v", err)
	}

	mq := ResolveMergeQueueConfig(townRoot, "gastown")
	if mq == nil {
		t.Fatal("ResolveMergeQueueConfig() = nil, want non-nil")
	}
	if mq.TestCommand != "make test-repo" {
		t.Errorf("TestCommand = %q, want %q (repo/local merge unaffected by absent rig-root floor)", mq.TestCommand, "make test-repo")
	}
}

// TestResolveMergeQueueConfig_Editorial guards the om editorial gate's
// config resolution (gt-wsg7, Task 2 of the om-gate coverage plan): a
// rig-root floor that sets editorial.required=true must survive a repo
// tier that omits editorial entirely, and the resolved block must come
// back with its other fields defaulted (scripts/om-gate.sh, max_attempts=5,
// review_parallelism=3).
func TestResolveMergeQueueConfig_Editorial(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown")
	repoRoot := filepath.Join(rigDir, "mayor", "rig")
	gastownDir := filepath.Join(repoRoot, ".gastown")
	if err := os.MkdirAll(gastownDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", gastownDir, err)
	}

	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "gastown",
  "git_url": "https://github.com/sloanahrens/gastown.git",
  "merge_queue": {
    "editorial": {"required": true}
  }
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}

	// Repo tier omits editorial entirely — the rig-root floor must survive.
	repoSettings := `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {"test_command": "make test-repo"}
}`
	if err := os.WriteFile(filepath.Join(gastownDir, "settings.json"), []byte(repoSettings), 0o644); err != nil {
		t.Fatalf("write repo settings.json: %v", err)
	}

	mq := ResolveMergeQueueConfig(townRoot, "gastown")
	if mq == nil {
		t.Fatal("ResolveMergeQueueConfig() = nil, want non-nil")
	}
	if mq.Editorial == nil {
		t.Fatal("Editorial = nil, want rig-root floor to survive repo tier omitting it")
	}
	if !mq.Editorial.Required {
		t.Error("Editorial.Required = false, want true (from rig-root floor)")
	}
}

// TestResolveMergeQueueConfig_NoConfig verifies the nil-townRoot/nil-rigName
// and no-config-anywhere cases return nil.
func TestResolveMergeQueueConfig_NoConfig(t *testing.T) {
	t.Parallel()
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

// TestResolveForgejoConfig_Precedence mirrors
// TestResolveMergeQueueConfig_Precedence for the merge_queue.forgejo block
// (gt-fn9e6.3). The reader delegates to ResolveMergeQueueConfig, so this test
// guards both that it reads the right block and that the operator-tier
// precedence holds for it: the rig root config.json is the floor and the
// rig-local settings/config.json has the final say. Fields the more specific
// tier leaves unset fall through, and bots overlay role by role.
func TestResolveForgejoConfig_Precedence(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "mango")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", settingsDir, err)
	}

	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "mango",
  "git_url": "https://github.com/sloanahrens/mango.git",
  "default_branch": "main",
  "merge_queue": {
    "forgejo": {
      "remote_url": "https://forgejo.example/floor/mango",
      "gate_workflow": "gate",
      "bots": {"polecat": "floor-polecat", "landing": "floor-landing"},
      "mirror_target": "git@github.com:sloanahrens/mango.git"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}

	localSettings := `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "forgejo": {"gate_workflow": "local-gate", "bots": {"landing": "local-landing"}}
  }
}`
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(localSettings), 0o644); err != nil {
		t.Fatalf("write settings/config.json: %v", err)
	}

	fc := ResolveForgejoConfig(townRoot, "mango")
	if fc == nil {
		t.Fatal("ResolveForgejoConfig() = nil, want non-nil")
	}
	if fc.RemoteURL != "https://forgejo.example/floor/mango" {
		t.Errorf("RemoteURL = %q, want the rig-root floor value", fc.RemoteURL)
	}
	if fc.GateWorkflowName() != "local-gate" {
		t.Errorf("GateWorkflow = %q, want the rig-local override", fc.GateWorkflow)
	}
	if fc.MirrorTarget != "git@github.com:sloanahrens/mango.git" {
		t.Errorf("MirrorTarget = %q, want the unset-in-local floor value", fc.MirrorTarget)
	}
	if got := fc.BotLogin("polecat"); got != "floor-polecat" {
		t.Errorf("polecat bot = %q, want the floor value (untouched by the lower tier)", got)
	}
	if got := fc.BotLogin("landing"); got != "local-landing" {
		t.Errorf("landing bot = %q, want the rig-local override", got)
	}
}

// TestResolveForgejoConfig_RepoBlockIgnored is the gt-fn9e6.14 trust-boundary
// test: a merge_queue.forgejo block in the repo-committed .gastown/settings.json
// names the remote a bot token is sent to and the logins the landing creator
// check trusts, so no field of it may reach ResolveForgejoConfig. The other
// merge_queue fields must still take their normal precedence, and the block
// must be reported once rather than applied.
func TestResolveForgejoConfig_RepoBlockIgnored(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "mango")
	repoRoot := filepath.Join(rigDir, "mayor", "rig")
	gastownDir := filepath.Join(repoRoot, ".gastown")
	if err := os.MkdirAll(gastownDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", gastownDir, err)
	}

	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "mango",
  "git_url": "https://github.com/sloanahrens/mango.git",
  "default_branch": "main",
  "merge_queue": {
    "test_command": "make test-floor",
    "forgejo": {
      "remote_url": "https://forgejo.example/floor/mango",
      "bots": {"landing": "floor-landing"},
      "mirror_target": "git@github.com:sloanahrens/mango.git"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}

	repoSettings := `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "test_command": "make test-repo",
    "forgejo": {
      "remote_url": "https://attacker.example/mango",
      "bots": {"landing": "attacker", "viewer": "attacker-viewer"},
      "mirror_target": "git@github.com:attacker/mango.git"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(gastownDir, "settings.json"), []byte(repoSettings), 0o644); err != nil {
		t.Fatalf("write repo settings.json: %v", err)
	}
	repoSettingsPath := filepath.Join(gastownDir, "settings.json")

	fc := ResolveForgejoConfig(townRoot, "mango")
	if fc == nil {
		t.Fatal("ResolveForgejoConfig() = nil, want the rig-root floor block")
	}
	if fc.RemoteURL != "https://forgejo.example/floor/mango" {
		t.Errorf("RemoteURL = %q, want the rig-root floor value (repo tier ignored)", fc.RemoteURL)
	}
	if fc.MirrorTarget != "git@github.com:sloanahrens/mango.git" {
		t.Errorf("MirrorTarget = %q, want the rig-root floor value (repo tier ignored)", fc.MirrorTarget)
	}
	if got := fc.BotLogin("landing"); got != "floor-landing" {
		t.Errorf("landing bot = %q, want the rig-root floor value (repo tier ignored)", got)
	}
	if got := fc.BotLogin("viewer"); got != "" {
		t.Errorf("viewer bot = %q, want empty (repo tier ignored)", got)
	}

	// The strip must not disturb how the other merge_queue fields merge.
	if mq := ResolveMergeQueueConfig(townRoot, "mango"); mq == nil || mq.TestCommand != "make test-repo" {
		t.Errorf("TestCommand = %v, want %q (repo tier still wins for other fields)", mq, "make test-repo")
	}

	if !RepoForgejoIgnoredWarned(repoSettingsPath) {
		t.Errorf("RepoForgejoIgnoredWarned(%s) = false, want the ignored block reported", repoSettingsPath)
	}
}

// TestResolveForgejoConfig_RepoBlockOnlyIsNotAForgejoRig pins the other half
// of the gt-fn9e6.14 rule: when only the repo tier carries a forgejo block,
// the rig has no Forgejo config at all, so a landed commit cannot turn one on.
func TestResolveForgejoConfig_RepoBlockOnlyIsNotAForgejoRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "mango")
	repoRoot := filepath.Join(rigDir, "mayor", "rig")
	gastownDir := filepath.Join(repoRoot, ".gastown")
	if err := os.MkdirAll(gastownDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", gastownDir, err)
	}

	rigConfig := `{"type":"rig","version":1,"name":"mango",
		"git_url":"https://github.com/sloanahrens/mango.git",
		"merge_queue":{"test_command":"make test"}}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}

	repoSettings := `{"type":"rig-settings","version":1,
		"merge_queue":{"forgejo":{"remote_url":"https://attacker.example/mango"}}}`
	if err := os.WriteFile(filepath.Join(gastownDir, "settings.json"), []byte(repoSettings), 0o644); err != nil {
		t.Fatalf("write repo settings.json: %v", err)
	}

	if got := ResolveForgejoConfig(townRoot, "mango"); got != nil {
		t.Errorf("ResolveForgejoConfig() = %+v, want nil (repo tier cannot define the block)", got)
	}
}

// TestResolveForgejoConfig_Absent verifies that a rig with no forgejo block
// at any tier resolves to nil, so a caller can tell "not a Forgejo rig"
// apart from a half-configured one.
func TestResolveForgejoConfig_Absent(t *testing.T) {
	t.Parallel()
	if got := ResolveForgejoConfig("", "mango"); got != nil {
		t.Errorf("ResolveForgejoConfig(empty townRoot) = %+v, want nil", got)
	}
	if got := ResolveForgejoConfig("/tmp", ""); got != nil {
		t.Errorf("ResolveForgejoConfig(empty rigName) = %+v, want nil", got)
	}

	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "mango")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig dir: %v", err)
	}
	if got := ResolveForgejoConfig(townRoot, "mango"); got != nil {
		t.Errorf("ResolveForgejoConfig() with no config files = %+v, want nil", got)
	}

	// A rig with merge_queue but no forgejo block is not a Forgejo rig.
	rigConfig := `{"type":"rig","version":1,"name":"mango",
		"git_url":"https://github.com/sloanahrens/mango.git",
		"merge_queue":{"test_command":"make test"}}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}
	if got := ResolveForgejoConfig(townRoot, "mango"); got != nil {
		t.Errorf("ResolveForgejoConfig() with merge_queue but no forgejo = %+v, want nil", got)
	}
}
