package doctor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hooks"
)

// scaffoldWorkspace creates a minimal town workspace in a temp directory with
// the given role agents configured. Returns the town root path.
func scaffoldWorkspace(t *testing.T, roleAgents map[string]string) string {
	t.Helper()
	tmpDir := t.TempDir()
	townRoot := filepath.Join(tmpDir, "town")

	// Required workspace structure (deacon/dogs is the dog kennel; the rig
	// crew worktree is the town's one Claude settings target once the mayor
	// role is retired).
	for _, dir := range []string{"mayor", filepath.Join("deacon", "dogs"), filepath.Join("myrig", "crew", "alice")} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// Workspace marker
	if err := os.WriteFile(
		filepath.Join(townRoot, "mayor", "town.json"),
		[]byte(`{"type":"town","version":1,"name":"test"}`),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	// Town settings with role agents
	townSettings := config.NewTownSettings()
	townSettings.RoleAgents = roleAgents
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatal(err)
	}

	return townRoot
}

// testExpectedHooks stands in for hooks.ComputeExpected, which reads the base
// and overrides under ~/.gt: every target gets its own session-start command,
// so a target synced for another would read as out of sync.
func testExpectedHooks(target string) (*hooks.HooksConfig, error) {
	return &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "echo " + target}}},
		},
	}, nil
}

// newTestHooksSyncCheck is a HooksSyncCheck expecting testExpectedHooks.
func newTestHooksSyncCheck() *HooksSyncCheck {
	c := NewHooksSyncCheck()
	c.computeExpected = testExpectedHooks
	return c
}

// syncAllClaudeTargets creates in-sync .claude/settings.json for every
// Claude target that DiscoverTargets would find. This prevents false
// positives from unrelated Claude targets in template agent tests.
func syncAllClaudeTargets(t *testing.T, townRoot string) {
	t.Helper()
	targets, err := hooks.DiscoverTargets(townRoot)
	if err != nil {
		t.Fatalf("DiscoverTargets: %v", err)
	}
	for _, target := range targets {
		expected, err := testExpectedHooks(target.Key)
		if err != nil {
			t.Fatalf("ComputeExpected(%s): %v", target.Key, err)
		}
		settings := &hooks.SettingsJSON{Hooks: *expected}
		data, err := hooks.MarshalSettings(settings)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(target.Path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target.Path, append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// writePassingSyncReport writes a sync-report.json recording a verified
// canary live-fire pair, the deployment record 'gt hooks sync' writes on a
// real successful run. Tests that assert StatusOK for an in-sync workspace
// need this: in-sync files alone are no longer sufficient (claude-41j.1
// D7/D8) — the hooks-sync check downgrades to StatusSkipped without it.
func writePassingSyncReport(t *testing.T, townRoot string) {
	t.Helper()
	report := &hooks.SyncReport{
		Timestamp: time.Now().UTC(),
		Canary: hooks.SyncReportCanary{
			Target:       "myrig/crew",
			SettingsPath: filepath.Join(townRoot, "myrig", "crew", ".claude", "settings.json"),
			Blocked:      hooks.SyncReportShape{Verdict: hooks.SyncReportPass, Detail: "blocked ok"},
			Allowed:      hooks.SyncReportShape{Verdict: hooks.SyncReportPass, Detail: "allowed ok"},
		},
		Roles: map[string]hooks.SyncReportRole{},
	}
	if err := hooks.WriteSyncReport(townRoot, report); err != nil {
		t.Fatalf("WriteSyncReport: %v", err)
	}
}

func TestHooksSyncCheck_ClaudeTargetInSync(t *testing.T) {
	t.Parallel()
	townRoot := scaffoldWorkspace(t, nil)

	// Create a rig with a crew worktree
	worktree := filepath.Join(townRoot, "myrig", "crew", "alice")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}

	// Sync ALL Claude targets (the crew worktree)
	syncAllClaudeTargets(t, townRoot)
	writePassingSyncReport(t, townRoot)

	check := newTestHooksSyncCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for in-sync Claude targets, got %v: %s", result.Status, result.Message)
		for _, d := range result.Details {
			t.Logf("  detail: %s", d)
		}
	}
}

func TestHooksSyncCheck_ClaudeTargetMissingPromptDefaults(t *testing.T) {
	t.Parallel()
	townRoot := scaffoldWorkspace(t, nil)
	syncAllClaudeTargets(t, townRoot)

	targetPath := filepath.Join(townRoot, "myrig", "crew", ".claude", "settings.json")
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "skipDangerousModePermissionPrompt")
	delete(raw, "hasCompletedOnboarding")
	delete(raw, "theme")
	delete(raw, "permissions")
	data, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	check := newTestHooksSyncCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning for missing prompt defaults, got %v: %s", result.Status, result.Message)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}
	check = newTestHooksSyncCheck()
	result = check.Run(ctx)
	// Fix() mechanically repairs the settings file directly — it doesn't run
	// 'gt hooks sync's canary live-fire pair, so no sync report exists yet.
	// Files matching is no longer sufficient for StatusOK (claude-41j.1
	// D7/D8): an auto-fixed file hasn't been live-fire verified either.
	if result.Status != StatusSkipped {
		t.Fatalf("expected StatusSkipped after fix (no sync report written by Fix), got %v: %s", result.Status, result.Message)
	}

	writePassingSyncReport(t, townRoot)
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK once a sync report exists, got %v: %s", result.Status, result.Message)
	}
}

func TestHooksSyncCheck_Fix_PreservesClaudePath(t *testing.T) {
	t.Parallel()
	townRoot := scaffoldWorkspace(t, nil)

	// Sync all Claude targets first (creates in-sync settings for the crew worktree)
	syncAllClaudeTargets(t, townRoot)

	// THEN overwrite the crew worktree's settings with stale hooks but a custom editorMode
	crewClaudeDir := filepath.Join(townRoot, "myrig", "crew", ".claude")
	stale := &hooks.SettingsJSON{
		EditorMode: "vim",
		Hooks: hooks.HooksConfig{
			SessionStart: []hooks.HookEntry{
				{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "old-cmd"}}},
			},
		},
	}
	data, err := hooks.MarshalSettings(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crewClaudeDir, "settings.json"), append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}

	check := newTestHooksSyncCheck()
	ctx := &CheckContext{TownRoot: townRoot}

	// Run to detect out-of-sync
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning before fix, got %v: %s", result.Status, result.Message)
	}

	// Fix
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	// Verify editorMode was preserved (merge path, not overwrite)
	settings, err := hooks.LoadSettings(filepath.Join(crewClaudeDir, "settings.json"))
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if settings.EditorMode != "vim" {
		t.Errorf("editorMode not preserved: got %q, want %q", settings.EditorMode, "vim")
	}
}

// TestHooksSyncCheck_NoSyncReport_Skipped pins the "Skipped when absent"
// requirement (claude-41j.1): every target can match what ComputeExpected
// would generate — proving the bytes are right — without proving the hooks
// they encode were ever live-fire verified. Without sync-report.json, the
// check must report StatusSkipped, never StatusOK.
func TestHooksSyncCheck_NoSyncReport_Skipped(t *testing.T) {
	t.Parallel()
	townRoot := scaffoldWorkspace(t, nil)
	syncAllClaudeTargets(t, townRoot)
	// Deliberately no writePassingSyncReport call.

	check := newTestHooksSyncCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	result := check.Run(ctx)

	if result.Status == StatusOK {
		t.Fatalf("must never report StatusOK without a sync report, got: %s", result.Message)
	}
	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped when sync report is absent, got %v: %s", result.Status, result.Message)
	}
}

// TestHooksSyncCheck_SyncReportCanaryFailed_Warning verifies that a present
// but unverified sync report (the canary's live-fire pair did not pass) is
// reported as a warning naming the pair result, not a silent pass.
func TestHooksSyncCheck_SyncReportCanaryFailed_Warning(t *testing.T) {
	t.Parallel()
	townRoot := scaffoldWorkspace(t, nil)
	syncAllClaudeTargets(t, townRoot)

	report := &hooks.SyncReport{
		Timestamp: time.Now().UTC(),
		Canary: hooks.SyncReportCanary{
			Target:       "myrig/crew",
			SettingsPath: filepath.Join(townRoot, "myrig", "crew", ".claude", "settings.json"),
			Blocked:      hooks.SyncReportShape{Verdict: hooks.SyncReportFail, Detail: "guard failed open"},
			Allowed:      hooks.SyncReportShape{Verdict: hooks.SyncReportPass, Detail: "allowed ok"},
		},
		Roles: map[string]hooks.SyncReportRole{},
	}
	if err := hooks.WriteSyncReport(townRoot, report); err != nil {
		t.Fatalf("WriteSyncReport: %v", err)
	}

	check := newTestHooksSyncCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for an unverified canary pair, got %v: %s", result.Status, result.Message)
	}
}

// TestHooksSyncCheck_UnloadableSettingsReportsOutOfSync verifies that a target
// whose settings.json cannot be parsed is reported by name as out of sync, not
// skipped into an all-in-sync result by a passing sync report.
func TestHooksSyncCheck_UnloadableSettingsReportsOutOfSync(t *testing.T) {
	t.Parallel()
	townRoot := scaffoldWorkspace(t, nil)
	syncAllClaudeTargets(t, townRoot)
	writePassingSyncReport(t, townRoot)

	targetPath := filepath.Join(townRoot, "myrig", "crew", ".claude", "settings.json")
	if err := os.WriteFile(targetPath, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}

	check := newTestHooksSyncCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning for an unloadable settings.json, got %v: %s", result.Status, result.Message)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "myrig/crew") && strings.Contains(d, "error loading") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a detail naming myrig/crew with a load error, got %v", result.Details)
	}
	if !strings.Contains(result.Message, "1 target(s) out of sync") {
		t.Errorf("message should count the unloadable target, got %q", result.Message)
	}
	if strings.Contains(result.FixHint, "to regenerate settings files") || !strings.Contains(result.FixHint, "will not rewrite") {
		t.Errorf("FixHint must not promise a repair Fix cannot make, got %q", result.FixHint)
	}

	// Fix must not overwrite what it could not read, and must say so.
	if err := check.Fix(&CheckContext{TownRoot: townRoot}); err == nil || !strings.Contains(err.Error(), "myrig/crew") {
		t.Errorf("Fix should return an error naming myrig/crew, got %v", err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{not json" {
		t.Errorf("Fix rewrote the unloadable settings.json: %q", got)
	}
}

// TestHooksSyncCheck_ExpectedComputeErrorReportsOutOfSync verifies that a
// target whose expected config cannot be computed is reported by name, not
// skipped into an all-in-sync result by a passing sync report.
func TestHooksSyncCheck_ExpectedComputeErrorReportsOutOfSync(t *testing.T) {
	t.Parallel()
	townRoot := scaffoldWorkspace(t, nil)
	syncAllClaudeTargets(t, townRoot)
	writePassingSyncReport(t, townRoot)

	check := newTestHooksSyncCheck()
	check.computeExpected = func(target string) (*hooks.HooksConfig, error) {
		if target == "myrig/crew" {
			return nil, errors.New("override unreadable")
		}
		return testExpectedHooks(target)
	}
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning when expected cannot be computed, got %v: %s", result.Status, result.Message)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "myrig/crew") && strings.Contains(d, "error computing expected") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a detail naming myrig/crew with a compute error, got %v", result.Details)
	}
	if err := check.Fix(&CheckContext{TownRoot: townRoot}); err == nil || !strings.Contains(err.Error(), "myrig/crew") {
		t.Errorf("Fix should return an error naming myrig/crew, got %v", err)
	}
}
