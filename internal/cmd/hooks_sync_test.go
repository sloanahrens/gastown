package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doctor"
	"github.com/steveyegge/gastown/internal/hooks"
)

// stubLiveFirePairRunner overrides liveFirePairRunner for the duration of
// the test so 'gt hooks sync' tests never spawn a real claude subprocess —
// this session's own harness proves claude is often in PATH, so tests must
// not rely on its absence to stay hermetic and fast.
func stubLiveFirePairRunner(t *testing.T, blocked, allowed doctor.LiveFireVerdict) {
	t.Helper()
	orig := liveFirePairRunner
	liveFirePairRunner = func(claudePath, settingsPath, label string) *doctor.LiveFirePairResult {
		return &doctor.LiveFirePairResult{
			Label:        label,
			SettingsPath: settingsPath,
			Blocked:      doctor.LiveFireShapeResult{Verdict: blocked, Detail: "stubbed blocked shape"},
			Allowed:      doctor.LiveFireShapeResult{Verdict: allowed, Detail: "stubbed allowed shape"},
		}
	}
	t.Cleanup(func() { liveFirePairRunner = orig })
}

func TestSyncTargetCreatesNew(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Save a base config
	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "echo hello"}}},
		},
	}
	if err := hooks.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	// Target that doesn't exist yet
	targetPath := filepath.Join(tmpDir, "test-rig", "crew", ".claude", "settings.json")
	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	result, err := syncTarget(target, false)
	if err != nil {
		t.Fatalf("syncTarget failed: %v", err)
	}

	if result != syncCreated {
		t.Errorf("expected syncCreated, got %d", result)
	}

	// Verify the file was written
	if _, err := os.Stat(targetPath); err != nil {
		t.Fatalf("settings.json not created: %v", err)
	}

	// Verify contents
	settings, err := hooks.LoadSettings(targetPath)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}

	if len(settings.Hooks.SessionStart) != 1 {
		t.Errorf("expected 1 SessionStart hook, got %d", len(settings.Hooks.SessionStart))
	}
	if settings.Hooks.SessionStart[0].Hooks[0].Command != "echo hello" {
		t.Errorf("unexpected command: %s", settings.Hooks.SessionStart[0].Hooks[0].Command)
	}
}

func TestSyncTargetUpdatesExisting(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Save a base config
	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "new-command"}}},
		},
	}
	if err := hooks.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	// Create existing settings.json with different hooks
	targetPath := filepath.Join(tmpDir, "test", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		t.Fatal(err)
	}

	existing := hooks.SettingsJSON{
		EditorMode: "vim",
		Hooks: hooks.HooksConfig{
			SessionStart: []hooks.HookEntry{
				{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "old-command"}}},
			},
		},
	}
	data, marshalErr := hooks.MarshalSettings(&existing)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err := os.WriteFile(targetPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	result, err := syncTarget(target, false)
	if err != nil {
		t.Fatalf("syncTarget failed: %v", err)
	}

	if result != syncUpdated {
		t.Errorf("expected syncUpdated, got %d", result)
	}

	// Verify the hooks were updated but editorMode preserved
	settings, err := hooks.LoadSettings(targetPath)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}

	if settings.EditorMode != "vim" {
		t.Errorf("editorMode not preserved: got %q", settings.EditorMode)
	}
	if settings.Hooks.SessionStart[0].Hooks[0].Command != "new-command" {
		t.Errorf("hooks not updated: got %s", settings.Hooks.SessionStart[0].Hooks[0].Command)
	}
}

func TestSyncTargetUnchanged(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Save a base config
	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "same-command"}}},
		},
	}
	if err := hooks.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	// Create existing settings.json with matching hooks
	targetPath := filepath.Join(tmpDir, "test", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		t.Fatal(err)
	}

	// Compute expected config for crew to ensure existing matches
	expected, err := hooks.ComputeExpected("crew")
	if err != nil {
		t.Fatalf("ComputeExpected failed: %v", err)
	}
	existing := hooks.SettingsJSON{
		Hooks: *expected,
	}
	data, marshalErr := hooks.MarshalSettings(&existing)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err := os.WriteFile(targetPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	result, err := syncTarget(target, false)
	if err != nil {
		t.Fatalf("syncTarget failed: %v", err)
	}

	if result != syncUnchanged {
		t.Errorf("expected syncUnchanged, got %d", result)
	}
}

func TestSyncTargetUpdatesExistingPromptDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	targetPath := filepath.Join(tmpDir, "test", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		t.Fatal(err)
	}

	expected, err := hooks.ComputeExpected("crew")
	if err != nil {
		t.Fatalf("ComputeExpected failed: %v", err)
	}
	rawHooks, err := json.Marshal(expected)
	if err != nil {
		t.Fatalf("marshal hooks: %v", err)
	}
	existing := map[string]json.RawMessage{
		"customSentinel": json.RawMessage(`true`),
		"hooks":          rawHooks,
	}
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := os.WriteFile(targetPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	result, err := syncTarget(target, false)
	if err != nil {
		t.Fatalf("syncTarget failed: %v", err)
	}
	if result != syncUpdated {
		t.Fatalf("expected syncUpdated, got %d", result)
	}

	data, err = os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings are not valid JSON: %v", err)
	}
	if got, ok := settings["customSentinel"].(bool); !ok || !got {
		t.Fatalf("customSentinel = %v, want true", settings["customSentinel"])
	}
	if got, ok := settings["hasCompletedOnboarding"].(bool); !ok || !got {
		t.Fatalf("hasCompletedOnboarding = %v, want true", settings["hasCompletedOnboarding"])
	}
	permissions, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions = %T, want object", settings["permissions"])
	}
	if got := permissions["defaultMode"]; got != "bypassPermissions" {
		t.Fatalf("permissions.defaultMode = %v, want bypassPermissions", got)
	}
}

func TestSyncTargetDryRun(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Save a base config
	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "test"}}},
		},
	}
	if err := hooks.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "test", ".claude", "settings.json")
	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	// Dry run should not create the file
	result, err := syncTarget(target, true)
	if err != nil {
		t.Fatalf("syncTarget dry-run failed: %v", err)
	}

	if result != syncCreated {
		t.Errorf("expected syncCreated (dry-run), got %d", result)
	}

	// File should NOT exist
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Error("dry-run should not create file")
	}
}

func TestSyncTargetSetsEnabledPlugins(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "test"}}},
		},
	}
	if err := hooks.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "test", ".claude", "settings.json")
	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	if _, err := syncTarget(target, false); err != nil {
		t.Fatalf("syncTarget failed: %v", err)
	}

	settings, err := hooks.LoadSettings(targetPath)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}

	if settings.EnabledPlugins == nil {
		t.Fatal("enabledPlugins should be set")
	}
	if settings.EnabledPlugins["beads@beads-marketplace"] != false {
		t.Error("beads@beads-marketplace should be disabled")
	}
}

func TestSyncTargetCreatesClaudePromptDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	targetPath := filepath.Join(tmpDir, "test-rig", "crew", ".claude", "settings.json")
	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	if _, err := syncTarget(target, false); err != nil {
		t.Fatalf("syncTarget failed: %v", err)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if strings.Contains(string(data), "export PATH=") {
		t.Fatal("synced settings contain stale export PATH marker")
	}
	if strings.Contains(string(data), "{{GT_BIN}}") {
		t.Fatal("synced settings contain unresolved {{GT_BIN}} placeholder")
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings are not valid JSON: %v", err)
	}
	if got, ok := settings["skipDangerousModePermissionPrompt"].(bool); !ok || !got {
		t.Fatalf("skipDangerousModePermissionPrompt = %v, want true", settings["skipDangerousModePermissionPrompt"])
	}
	if got, ok := settings["hasCompletedOnboarding"].(bool); !ok || !got {
		t.Fatalf("hasCompletedOnboarding = %v, want true", settings["hasCompletedOnboarding"])
	}
	if got := settings["theme"]; got != "dark" {
		t.Fatalf("theme = %v, want dark", got)
	}
	permissions, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions = %T, want object", settings["permissions"])
	}
	if got := permissions["defaultMode"]; got != "bypassPermissions" {
		t.Fatalf("permissions.defaultMode = %v, want bypassPermissions", got)
	}
}

func TestRunHooksSyncFailsClosedOnIntegrityViolation(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	townRoot := filepath.Join(tmpDir, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", ".claude"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "deacon"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town","version":1,"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", ".claude", "settings.json"), []byte(`{"hooks":{"SessionStart":"bad"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "echo hello"}}},
		},
	}
	if err := hooks.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chdir(cwd)
	}()
	if err := os.Chdir(townRoot); err != nil {
		t.Fatal(err)
	}

	hooksSyncDryRun = false
	err = runHooksSync(nil, nil)
	if err == nil {
		t.Fatal("expected hooks sync to fail closed")
	}
	if !strings.Contains(err.Error(), "failed closed") {
		t.Fatalf("expected fail-closed error, got: %v", err)
	}
}

func TestRunHooksSyncNonClaudeAgent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Put a dummy opencode binary on PATH so agent resolution doesn't fall back to claude.
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	townRoot := filepath.Join(tmpDir, "town")

	// Scaffold workspace: mayor, deacon, and a rig with a crew worktree
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "deacon"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "myrig", "crew", "alice"), 0755); err != nil {
		t.Fatal(err)
	}

	// Workspace marker
	if err := os.WriteFile(
		filepath.Join(townRoot, "mayor", "town.json"),
		[]byte(`{"type":"town","version":1,"name":"test"}`),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	// Configure crew role to use opencode
	townSettings := config.NewTownSettings()
	townSettings.RoleAgents = map[string]string{"crew": "opencode"}
	// Register opencode as a custom agent so resolution bypasses binary validation.
	// fillRuntimeDefaults will auto-fill hooks config from the opencode preset.
	townSettings.Agents = map[string]*config.RuntimeConfig{
		"opencode": {
			Provider: "opencode",
			Command:  "opencode",
		},
	}
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatal(err)
	}

	// Base hooks config (needed for Claude targets to not error)
	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "echo test"}}},
		},
	}
	if err := hooks.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(townRoot); err != nil {
		t.Fatal(err)
	}

	stubLiveFirePairRunner(t, doctor.LiveFirePass, doctor.LiveFirePass)
	hooksSyncDryRun = false
	if err := runHooksSync(nil, nil); err != nil {
		t.Fatalf("runHooksSync failed: %v", err)
	}

	// A successful run with a passing canary pair writes the deployment
	// record doctor hooks-sync reads back (claude-41j.1 D7/D8).
	if _, err := hooks.ReadSyncReport(townRoot); err != nil {
		t.Errorf("expected sync-report.json to be written on success: %v", err)
	}

	// Verify OpenCode plugin was synced to the worktree (not the parent)
	pluginPath := filepath.Join(townRoot, "myrig", "crew", "alice", ".opencode", "plugins", "gastown.js")
	if _, err := os.Stat(pluginPath); os.IsNotExist(err) {
		t.Error("opencode plugin not created in worktree alice")
	}

	// Verify it was NOT created in the parent (crew/) since useSettingsDir=false
	parentPlugin := filepath.Join(townRoot, "myrig", "crew", ".opencode", "plugins", "gastown.js")
	if _, err := os.Stat(parentPlugin); !os.IsNotExist(err) {
		t.Error("opencode plugin should not be in the parent crew/ directory")
	}
}

func TestRunHooksSyncNonClaudeAgentDryRun(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	townRoot := filepath.Join(tmpDir, "town")

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "deacon"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "myrig", "crew", "alice"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(townRoot, "mayor", "town.json"),
		[]byte(`{"type":"town","version":1,"name":"test"}`),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	townSettings := config.NewTownSettings()
	townSettings.RoleAgents = map[string]string{"crew": "opencode"}
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatal(err)
	}

	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "echo test"}}},
		},
	}
	if err := hooks.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(townRoot); err != nil {
		t.Fatal(err)
	}

	hooksSyncDryRun = true
	defer func() { hooksSyncDryRun = false }()
	if err := runHooksSync(nil, nil); err != nil {
		t.Fatalf("runHooksSync dry-run failed: %v", err)
	}

	// Dry run should NOT create the file
	pluginPath := filepath.Join(townRoot, "myrig", "crew", "alice", ".opencode", "plugins", "gastown.js")
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Error("dry-run should not create opencode plugin file")
	}
}

func TestRunHooksSyncNonClaudeAgentNestedPolecatWorktree(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	townRoot := filepath.Join(tmpDir, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "deacon"), 0755); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(townRoot, "myrig", "polecats", "fury", "gastown")
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(townRoot, "mayor", "town.json"),
		[]byte(`{"type":"town","version":1,"name":"test"}`),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	townSettings := config.NewTownSettings()
	townSettings.RoleAgents = map[string]string{"polecat": "opencode"}
	townSettings.Agents = map[string]*config.RuntimeConfig{
		"opencode": {
			Provider: "opencode",
			Command:  "opencode",
		},
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatal(err)
	}

	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "echo test"}}},
		},
	}
	if err := hooks.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(townRoot); err != nil {
		t.Fatal(err)
	}

	stubLiveFirePairRunner(t, doctor.LiveFirePass, doctor.LiveFirePass)
	hooksSyncDryRun = false
	if err := runHooksSync(nil, nil); err != nil {
		t.Fatalf("runHooksSync failed: %v", err)
	}

	pluginPath := filepath.Join(worktree, ".opencode", "plugins", "gastown.js")
	if _, err := os.Stat(pluginPath); os.IsNotExist(err) {
		t.Fatalf("opencode plugin not created in nested polecat worktree %s", pluginPath)
	}

	wrongParentPath := filepath.Join(townRoot, "myrig", "polecats", "fury", ".opencode", "plugins", "gastown.js")
	if _, err := os.Stat(wrongParentPath); !os.IsNotExist(err) {
		t.Fatalf("opencode plugin should not be created in polecat slot parent %s", wrongParentPath)
	}
}

// scaffoldSyncWorkspace creates a minimal town (mayor, deacon, and one rig
// with a crew worktree) with a base hooks config, and chdirs into it,
// restoring the original cwd on test cleanup. Returns the town root.
func scaffoldSyncWorkspace(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	townRoot := filepath.Join(tmpDir, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "deacon"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "myrig", "crew", "alice"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(townRoot, "mayor", "town.json"),
		[]byte(`{"type":"town","version":1,"name":"test"}`),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "echo test"}}},
		},
	}
	if err := hooks.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(townRoot); err != nil {
		t.Fatal(err)
	}

	return townRoot
}

// TestRunHooksSyncCanaryFailurePreventsFanOut pins the acceptance criterion
// that a confirmed canary live-fire failure aborts before any other target
// is touched (claude-41j.1 D7/D8): the crew target — synced only after the
// canary in fan-out order — must never be written.
func TestRunHooksSyncCanaryFailurePreventsFanOut(t *testing.T) {
	townRoot := scaffoldSyncWorkspace(t)
	stubLiveFirePairRunner(t, doctor.LiveFireFail, doctor.LiveFirePass)

	hooksSyncDryRun = false
	err := runHooksSync(nil, nil)
	if err == nil {
		t.Fatal("expected hooks sync to abort on a failed canary live-fire pair")
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Errorf("expected an abort error naming the canary, got: %v", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(townRoot, "mayor", ".claude", "settings.json")) {
		t.Errorf("expected the canary settings path in the error, got: %v", err)
	}

	crewSettings := filepath.Join(townRoot, "myrig", "crew", ".claude", "settings.json")
	if _, statErr := os.Stat(crewSettings); !os.IsNotExist(statErr) {
		t.Error("fan-out target was synced despite a failed canary — fan-out should have stopped")
	}

	if _, err := hooks.ReadSyncReport(townRoot); !os.IsNotExist(err) {
		t.Errorf("expected no sync report to be written after an aborted sync, got err=%v", err)
	}
}

// TestRunHooksSyncCanaryInconclusiveProceeds verifies that an inconclusive
// canary result (could not prove either shape) is a warning, not an abort —
// only a confirmed failure blocks fan-out.
func TestRunHooksSyncCanaryInconclusiveProceeds(t *testing.T) {
	townRoot := scaffoldSyncWorkspace(t)
	stubLiveFirePairRunner(t, doctor.LiveFireInconclusive, doctor.LiveFirePass)

	hooksSyncDryRun = false
	if err := runHooksSync(nil, nil); err != nil {
		t.Fatalf("runHooksSync should proceed on an inconclusive (not failed) canary pair: %v", err)
	}

	crewSettings := filepath.Join(townRoot, "myrig", "crew", ".claude", "settings.json")
	if _, statErr := os.Stat(crewSettings); statErr != nil {
		t.Errorf("expected fan-out to proceed past an inconclusive canary: %v", statErr)
	}

	// An unverified (not Passed) canary still writes a report — it just
	// won't read back as StatusOK from the hooks-sync doctor check.
	report, err := hooks.ReadSyncReport(townRoot)
	if err != nil {
		t.Fatalf("expected a sync report to be written: %v", err)
	}
	if report.Canary.Passed() {
		t.Error("expected the recorded canary result not to be a full pass")
	}
}

// TestRunHooksSyncWritesReportWithEffectiveHookSet verifies the deployment
// record: on a fully successful run, sync-report.json records the canary's
// pair result and, per role, the effective hook set as rendered.
func TestRunHooksSyncWritesReportWithEffectiveHookSet(t *testing.T) {
	townRoot := scaffoldSyncWorkspace(t)
	stubLiveFirePairRunner(t, doctor.LiveFirePass, doctor.LiveFirePass)

	hooksSyncDryRun = false
	if err := runHooksSync(nil, nil); err != nil {
		t.Fatalf("runHooksSync failed: %v", err)
	}

	report, err := hooks.ReadSyncReport(townRoot)
	if err != nil {
		t.Fatalf("ReadSyncReport: %v", err)
	}
	if !report.Canary.Passed() {
		t.Errorf("expected a passed canary, got blocked=%s allowed=%s", report.Canary.Blocked.Verdict, report.Canary.Allowed.Verdict)
	}
	if report.Canary.Target != "mayor" {
		t.Errorf("expected mayor as the deterministic canary, got %q", report.Canary.Target)
	}
	if report.Timestamp.IsZero() {
		t.Error("expected a non-zero timestamp")
	}

	mayorRole, ok := report.Roles["mayor"]
	if !ok {
		t.Fatal("expected the mayor role's effective hook set to be recorded")
	}
	if len(mayorRole.Hooks.SessionStart) == 0 {
		t.Error("expected mayor's recorded hook set to include the base SessionStart entry")
	}

	crewRole, ok := report.Roles["myrig/crew"]
	if !ok {
		t.Fatal("expected the myrig/crew role's effective hook set to be recorded")
	}
	if crewRole.Rig != "myrig" {
		t.Errorf("expected rig %q, got %q", "myrig", crewRole.Rig)
	}
}
