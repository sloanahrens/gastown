package cmd

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doctor"
	"github.com/steveyegge/gastown/internal/hooks"
)

// stubLiveFire returns a canary probe answering blocked and allowed, so
// 'gt hooks sync' tests never spawn a real claude.
func stubLiveFire(blocked, allowed doctor.LiveFireVerdict) func(hooks.Target) (*doctor.LiveFirePairResult, error) {
	return func(target hooks.Target) (*doctor.LiveFirePairResult, error) {
		return &doctor.LiveFirePairResult{
			Label:        target.DisplayKey(),
			SettingsPath: target.Path,
			Blocked:      doctor.LiveFireShapeResult{Verdict: blocked, Detail: "stubbed blocked shape"},
			Allowed:      doctor.LiveFireShapeResult{Verdict: allowed, Detail: "stubbed allowed shape"},
		}, nil
	}
}

// noLiveFire is a canary probe a test does not expect to run.
func noLiveFire(t *testing.T) func(hooks.Target) (*doctor.LiveFirePairResult, error) {
	return func(target hooks.Target) (*doctor.LiveFirePairResult, error) {
		t.Errorf("canary live-fire ran against %s", target.Path)
		return nil, errors.New("unexpected live-fire")
	}
}

// newHooksSyncRun is a sync of townRoot with hook configs under home.
func newHooksSyncRun(townRoot, home string, dryRun bool, liveFire func(hooks.Target) (*doctor.LiveFirePairResult, error)) hooksSyncRun {
	return hooksSyncRun{
		townRoot: townRoot,
		dryRun:   dryRun,
		home:     hooks.HomeAt(home),
		liveFire: liveFire,
		out:      io.Discard,
		now:      func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
	}
}

func TestSyncTargetCreatesNew(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := hooks.HomeAt(tmpDir)

	// Save a base config
	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "echo hello"}}},
		},
	}
	if err := home.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	// Target that doesn't exist yet
	targetPath := filepath.Join(tmpDir, "test-rig", "crew", ".claude", "settings.json")
	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	result, err := syncTargetIn(home, target, false)
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
	t.Parallel()
	tmpDir := t.TempDir()
	home := hooks.HomeAt(tmpDir)

	// Save a base config
	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "new-command"}}},
		},
	}
	if err := home.SaveBase(base); err != nil {
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

	result, err := syncTargetIn(home, target, false)
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
	t.Parallel()
	tmpDir := t.TempDir()
	home := hooks.HomeAt(tmpDir)

	// Save a base config
	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "same-command"}}},
		},
	}
	if err := home.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	// Create existing settings.json with matching hooks
	targetPath := filepath.Join(tmpDir, "test", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		t.Fatal(err)
	}

	// Compute expected config for crew to ensure existing matches
	expected, err := home.ComputeExpected("crew")
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

	result, err := syncTargetIn(home, target, false)
	if err != nil {
		t.Fatalf("syncTarget failed: %v", err)
	}

	if result != syncUnchanged {
		t.Errorf("expected syncUnchanged, got %d", result)
	}
}

func TestSyncTargetUpdatesExistingPromptDefaults(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := hooks.HomeAt(tmpDir)

	targetPath := filepath.Join(tmpDir, "test", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		t.Fatal(err)
	}

	expected, err := home.ComputeExpected("crew")
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

	result, err := syncTargetIn(home, target, false)
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
	t.Parallel()
	tmpDir := t.TempDir()
	home := hooks.HomeAt(tmpDir)

	// Save a base config
	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "test"}}},
		},
	}
	if err := home.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "test", ".claude", "settings.json")
	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	// Dry run should not create the file
	result, err := syncTargetIn(home, target, true)
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
	t.Parallel()
	tmpDir := t.TempDir()
	home := hooks.HomeAt(tmpDir)

	base := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "test"}}},
		},
	}
	if err := home.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "test", ".claude", "settings.json")
	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	if _, err := syncTargetIn(home, target, false); err != nil {
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
	t.Parallel()
	tmpDir := t.TempDir()
	home := hooks.HomeAt(tmpDir)

	targetPath := filepath.Join(tmpDir, "test-rig", "crew", ".claude", "settings.json")
	target := hooks.Target{
		Path: targetPath,
		Key:  "crew",
		Role: "crew",
	}

	if _, err := syncTargetIn(home, target, false); err != nil {
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
	t.Parallel()
	tmpDir := t.TempDir()
	home := hooks.HomeAt(tmpDir)

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
	if err := home.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	err := newHooksSyncRun(townRoot, tmpDir, false, noLiveFire(t)).run()
	if err == nil {
		t.Fatal("expected hooks sync to fail closed")
	}
	if !strings.Contains(err.Error(), "failed closed") {
		t.Fatalf("expected fail-closed error, got: %v", err)
	}
}

// scaffoldSyncWorkspace creates a minimal town (mayor, deacon, and one rig
// with a crew worktree) and a home holding a base hooks config. Returns the
// town root and the home.
func scaffoldSyncWorkspace(t *testing.T) (townRoot, homeDir string) {
	t.Helper()
	tmpDir := t.TempDir()
	home := hooks.HomeAt(tmpDir)

	townRoot = filepath.Join(tmpDir, "town")
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
	if err := home.SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	return townRoot, tmpDir
}

// TestRunHooksSyncCanaryFailurePreventsFanOut pins the acceptance criterion
// that a confirmed canary live-fire failure aborts before any other target
// is touched (claude-41j.1 D7/D8): the crew target — synced only after the
// canary in fan-out order — must never be written.
func TestRunHooksSyncCanaryFailurePreventsFanOut(t *testing.T) {
	t.Parallel()
	townRoot, home := scaffoldSyncWorkspace(t)
	r := newHooksSyncRun(townRoot, home, false, stubLiveFire(doctor.LiveFireFail, doctor.LiveFirePass))
	err := r.run()
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
	t.Parallel()
	townRoot, home := scaffoldSyncWorkspace(t)
	r := newHooksSyncRun(townRoot, home, false, stubLiveFire(doctor.LiveFireInconclusive, doctor.LiveFirePass))
	if err := r.run(); err != nil {
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
	t.Parallel()
	townRoot, home := scaffoldSyncWorkspace(t)
	r := newHooksSyncRun(townRoot, home, false, stubLiveFire(doctor.LiveFirePass, doctor.LiveFirePass))
	if err := r.run(); err != nil {
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
