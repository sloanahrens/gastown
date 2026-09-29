package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/refinery"
)

func TestBuildWitnessPatrolVars_NilContext(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{}
	vars := buildWitnessPatrolVars(ctx)
	if len(vars) != 0 {
		t.Errorf("expected empty vars for nil context, got %v", vars)
	}
}

func TestBuildWitnessPatrolVars_InjectsRigAndPrefix(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildWitnessPatrolVars(ctx)
	if len(vars) != 2 {
		t.Fatalf("expected 2 vars (rig, prefix), got %v", vars)
	}
	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}
	if got := varMap["rig"]; got != "testrig" {
		t.Errorf("rig = %q, want %q", got, "testrig")
	}
	if got := varMap["prefix"]; got != "gt" {
		t.Errorf("prefix = %q, want %q (default fallback)", got, "gt")
	}
}

func TestBuildRefineryPatrolVars_NilContext(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{}
	vars := buildRefineryPatrolVars(ctx)
	if len(vars) != 0 {
		t.Errorf("expected empty vars for nil context, got %v", vars)
	}
}

func TestBuildRefineryPatrolVars_MissingSettings(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(filepath.Join(rigDir, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)
	// rig and target_branch should always be present.
	if len(vars) != 2 {
		t.Errorf("expected 2 vars (rig, target_branch) when settings file missing, got %v", vars)
	}
	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}
	if got := varMap["rig"]; got != "testrig" {
		t.Errorf("rig = %q, want %q", got, "testrig")
	}
	if got := varMap["target_branch"]; got != "main" {
		t.Errorf("target_branch = %q, want %q", got, "main")
	}
}

func TestAutoSpawnPatrol_RefinerySafetyStoppedSkipsWispCreate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock bd script uses POSIX shell")
	}
	townRoot := setupRefinerySafetyStopTown(t)
	logPath := installRefinerySafetyStopMockBD(t)

	_, err := autoSpawnPatrol(PatrolConfig{
		RoleName:      "refinery",
		PatrolMolName: constants.MolRefineryPatrol,
		BeadsDir:      townRoot,
		Assignee:      "testrig/refinery",
	})
	if !errors.Is(err, refinery.ErrSafetyStopped) {
		t.Fatalf("autoSpawnPatrol error = %v, want ErrSafetyStopped", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	if strings.Contains(string(logData), "mol wisp create") || strings.Contains(string(logData), "update ") {
		t.Fatalf("autoSpawnPatrol mutated patrol state despite safety stop; log:\n%s", logData)
	}
}

func setupRefinerySafetyStopTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{filepath.Join(townRoot, "mayor"), filepath.Join(townRoot, ".beads"), filepath.Join(townRoot, "testrig")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0o644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	return townRoot
}

func installRefinerySafetyStopMockBD(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
cmd=""
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) cmd="$arg"; break ;;
  esac
done
case "$cmd" in
  version)
    echo "bd test"
    ;;
  show)
    printf '%s\n' '[{"id":"gt-testrig-refinery","title":"Refinery","issue_type":"task","labels":["gt:agent","safety_stop:hq-vmrwr"],"status":"open","description":"role_type: refinery\nrig: testrig\nagent_state: idle"}]'
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func TestBuildRefineryPatrolVars_NilMergeQueue(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write settings with no merge_queue
	settings := config.RigSettings{
		Type:    "rig-settings",
		Version: 1,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)
	// rig and target_branch should always be present.
	if len(vars) != 2 {
		t.Errorf("expected 2 vars (rig, target_branch) when merge_queue is nil, got %v", vars)
	}
	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}
	if got := varMap["rig"]; got != "testrig" {
		t.Errorf("rig = %q, want %q", got, "testrig")
	}
	if got := varMap["target_branch"]; got != "main" {
		t.Errorf("target_branch = %q, want %q", got, "main")
	}
}

// TestBuildRefineryPatrolVars_ReadsRigRootMergeQueue reproduces gt-egiv
// finding 1: buildRefineryPatrolVars fed the refinery's own gate-command
// vars from settings/config.json only, so a rig configuring gate commands
// exclusively at rig-root onboarding time (gt-me9t) produced a refinery
// patrol that resolved NO commands — unable to re-verify a polecat's
// --pre-verified claim even though loadRigCommandVars had already surfaced
// those same commands to the polecat.
func TestBuildRefineryPatrolVars_ReadsRigRootMergeQueue(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "merge_queue": {
    "build_command": "make build",
    "test_command": "make test",
    "lint_command": "make lint"
  }
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)
	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}
	want := map[string]string{
		"build_command": "make build",
		"test_command":  "make test",
		"lint_command":  "make lint",
	}
	for key, wantVal := range want {
		if gotVal := varMap[key]; gotVal != wantVal {
			t.Errorf("%s = %q, want %q (rig-root merge_queue floor invisible to refinery patrol vars; vars: %v)", key, gotVal, wantVal, vars)
		}
	}
}

func TestBuildRefineryPatrolVars_FullConfig(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write rig config.json with default_branch (source of truth for default branch)
	rigConfig := map[string]interface{}{"type": "rig", "version": 1, "name": "testrig"}
	rigData, _ := json.Marshal(rigConfig)
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), rigData, 0o644); err != nil {
		t.Fatal(err)
	}

	mq := config.DefaultMergeQueueConfig()
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	// DefaultMergeQueueConfig: refinery_enabled=true, auto_land=false, run_tests=true,
	// test_command="" (language-agnostic), target_branch="main" (from rig config),
	// delete_merged_branches=true, judgment_enabled=false, review_depth="standard"
	// merge_strategy is omitted when not explicitly set (formula default "direct" applies)
	// New commands (setup, typecheck, lint, build) default to empty = omitted
	// judgment_enabled defaults to false, review_depth defaults to "standard"
	// batch_enabled defaults to false, batch_min_age to "1h", batch_max to 12,
	// batch_min_count to 4 (gt-hqji)
	expected := map[string]string{
		"rig":                                 "testrig",
		"integration_branch_refinery_enabled": "true",
		"integration_branch_auto_land":        "false",
		"run_tests":                           "true",
		"target_branch":                       "main",
		"delete_merged_branches":              "true",
		"judgment_enabled":                    "false",
		"review_depth":                        "standard",
		"require_review":                      "false",
		"batch_enabled":                       "false",
		"batch_min_age":                       "1h",
		"batch_max":                           "12",
		"batch_min_count":                     "4",
	}

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	for key, want := range expected {
		got, ok := varMap[key]
		if !ok {
			t.Errorf("missing var %q", key)
			continue
		}
		if got != want {
			t.Errorf("var %q = %q, want %q", key, got, want)
		}
	}

	// Verify empty commands and unset strategy are NOT included
	for _, shouldBeAbsent := range []string{"setup_command", "typecheck_command", "lint_command", "build_command", "merge_strategy"} {
		if _, ok := varMap[shouldBeAbsent]; ok {
			t.Errorf("%q should be omitted when empty/unset", shouldBeAbsent)
		}
	}

	if len(vars) != len(expected) {
		t.Errorf("expected %d vars, got %d: %v", len(expected), len(vars), vars)
	}
}

func TestBuildRefineryPatrolVars_AllCommandsSet(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mq := config.DefaultMergeQueueConfig()
	mq.SetupCommand = "pnpm install"
	mq.TypecheckCommand = "tsc --noEmit"
	mq.LintCommand = "eslint ."
	mq.BuildCommand = "pnpm build"
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	// All configured commands should be present (test_command is empty by default)
	commandExpected := map[string]string{
		"setup_command":     "pnpm install",
		"typecheck_command": "tsc --noEmit",
		"lint_command":      "eslint .",
		"build_command":     "pnpm build",
	}
	for key, want := range commandExpected {
		got, ok := varMap[key]
		if !ok {
			t.Errorf("missing var %q", key)
			continue
		}
		if got != want {
			t.Errorf("var %q = %q, want %q", key, got, want)
		}
	}
}

func TestBuildRefineryPatrolVars_EmptyTestCommand(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	falseVal := false
	trueVal2 := true
	mq := &config.MergeQueueConfig{
		Enabled:              true,
		RunTests:             &falseVal,
		TestCommand:          "", // empty - should be omitted
		DeleteMergedBranches: &trueVal2,
	}
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	// test_command should not be present when empty
	if _, ok := varMap["test_command"]; ok {
		t.Error("test_command should be omitted when empty")
	}

	// All command vars should be omitted when empty
	for _, cmd := range []string{"setup_command", "typecheck_command", "lint_command", "build_command"} {
		if _, ok := varMap[cmd]; ok {
			t.Errorf("%q should be omitted when empty", cmd)
		}
	}

	// run_tests should be "false"
	if got := varMap["run_tests"]; got != "false" {
		t.Errorf("run_tests = %q, want %q", got, "false")
	}
}

func TestBuildRefineryPatrolVars_BoolFormat(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write rig config.json with default_branch = "develop"
	rigConfig := map[string]interface{}{"type": "rig", "version": 1, "name": "testrig", "default_branch": "develop"}
	rigData, _ := json.Marshal(rigConfig)
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), rigData, 0o644); err != nil {
		t.Fatal(err)
	}

	trueVal := true
	falseVal2 := false
	mq := &config.MergeQueueConfig{
		Enabled:                          true,
		IntegrationBranchAutoLand:        &trueVal,
		IntegrationBranchRefineryEnabled: &trueVal,
		RunTests:                         &trueVal,
		SetupCommand:                     "npm ci",
		TypecheckCommand:                 "tsc --noEmit",
		LintCommand:                      "eslint .",
		TestCommand:                      "make test",
		BuildCommand:                     "make build",
		DeleteMergedBranches:             &falseVal2,
	}
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	// Check bool format is "true"/"false" strings
	if got := varMap["integration_branch_auto_land"]; got != "true" {
		t.Errorf("integration_branch_auto_land = %q, want %q", got, "true")
	}
	if got := varMap["delete_merged_branches"]; got != "false" {
		t.Errorf("delete_merged_branches = %q, want %q", got, "false")
	}
	if got := varMap["target_branch"]; got != "develop" {
		t.Errorf("target_branch = %q, want %q", got, "develop")
	}
	if got := varMap["test_command"]; got != "make test" {
		t.Errorf("test_command = %q, want %q", got, "make test")
	}
	if got := varMap["setup_command"]; got != "npm ci" {
		t.Errorf("setup_command = %q, want %q", got, "npm ci")
	}
	if got := varMap["typecheck_command"]; got != "tsc --noEmit" {
		t.Errorf("typecheck_command = %q, want %q", got, "tsc --noEmit")
	}
	if got := varMap["lint_command"]; got != "eslint ." {
		t.Errorf("lint_command = %q, want %q", got, "eslint .")
	}
	if got := varMap["build_command"]; got != "make build" {
		t.Errorf("build_command = %q, want %q", got, "make build")
	}
}

func TestBuildRefineryPatrolVars_DefaultBranchWithoutMQ(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write rig config with custom default_branch but NO settings/config.json
	rigConfig := map[string]interface{}{
		"type": "rig", "version": 1, "name": "testrig",
		"default_branch": "gastown",
	}
	rigData, _ := json.Marshal(rigConfig)
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), rigData, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	// rig and target_branch must be present even without merge_queue settings.
	if len(vars) != 2 {
		t.Errorf("expected 2 vars (rig, target_branch), got %d: %v", len(vars), vars)
	}
	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}
	if got := varMap["rig"]; got != "testrig" {
		t.Errorf("rig = %q, want %q", got, "testrig")
	}
	if got := varMap["target_branch"]; got != "gastown" {
		t.Errorf("target_branch = %q, want %q (should read rig config even without MQ settings)", got, "gastown")
	}
}

func TestBuildRefineryPatrolVars_MergeStrategy(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mq := config.DefaultMergeQueueConfig()
	mq.MergeStrategy = "pr"
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	if got := varMap["merge_strategy"]; got != "pr" {
		t.Errorf("merge_strategy = %q, want %q (rig-level config must override formula default)", got, "pr")
	}
}

func TestBuildRefineryPatrolVars_MergeStrategyDefaultOmitted(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// MergeStrategy not set — should not be injected (formula default "direct" applies)
	mq := config.DefaultMergeQueueConfig()
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	// merge_strategy should be absent when not explicitly configured
	if _, ok := varMap["merge_strategy"]; ok {
		t.Error("merge_strategy should be omitted when not configured (let formula default apply)")
	}
}

func TestBuildRefineryPatrolVars_RequireReview(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mq := config.DefaultMergeQueueConfig()
	mq.MergeStrategy = "pr"
	requireReview := true
	mq.RequireReview = &requireReview
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	if got := varMap["require_review"]; got != "true" {
		t.Errorf("require_review = %q, want %q", got, "true")
	}
	if got := varMap["merge_strategy"]; got != "pr" {
		t.Errorf("merge_strategy = %q, want %q", got, "pr")
	}
}

// TestBuildRefineryPatrolVars_Editorial guards the om editorial gate vars
// (gt-wsg7): when a rig configures merge_queue.editorial, the refinery
// patrol formula must receive the resolved-and-defaulted block as vars.
func TestBuildRefineryPatrolVars_Editorial(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mq := config.DefaultMergeQueueConfig()
	mq.Editorial = &config.EditorialConfig{Required: true}
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	if got := varMap["editorial_required"]; got != "true" {
		t.Errorf("editorial_required = %q, want %q", got, "true")
	}
	if got := varMap["editorial_command"]; got != "scripts/om-gate.sh" {
		t.Errorf("editorial_command = %q, want default %q", got, "scripts/om-gate.sh")
	}
	if got := varMap["editorial_max_attempts"]; got != "5" {
		t.Errorf("editorial_max_attempts = %q, want default %q", got, "5")
	}
	if got := varMap["editorial_review_parallelism"]; got != "3" {
		t.Errorf("editorial_review_parallelism = %q, want default %q", got, "3")
	}
	if _, ok := varMap["editorial_min_version"]; ok {
		t.Error("editorial_min_version should be omitted when not configured")
	}
}

// TestBuildRefineryPatrolVars_NoEditorial guards the omitted-when-unset
// counterpart: a rig that never configures editorial gets no editorial_*
// vars at all (upstream behavior unchanged, gt-wsg7).
func TestBuildRefineryPatrolVars_NoEditorial(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mq := config.DefaultMergeQueueConfig()
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 && strings.HasPrefix(parts[0], "editorial_") {
			t.Errorf("unexpected %s in vars when editorial is not configured", parts[0])
		}
	}
}

// splitFirstEquals splits a string on the first '=' only.
func splitFirstEquals(s string) []string {
	idx := -1
	for i, c := range s {
		if c == '=' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return []string{s}
	}
	return []string{s[:idx], s[idx+1:]}
}

func TestPatrolRigName(t *testing.T) {
	t.Parallel()
	if got := patrolRigName(PatrolConfig{Assignee: "gastown/refinery"}); got != "gastown" {
		t.Fatalf("patrolRigName = %q, want gastown", got)
	}
	if got := patrolRigName(PatrolConfig{Assignee: "deacon"}); got != "" {
		t.Fatalf("patrolRigName without rig = %q, want empty", got)
	}
}

func TestRenderPatrolWispDescription_DeaconInlinesStepsAndVars(t *testing.T) {
	t.Parallel()
	desc, err := renderPatrolWispDescription(PatrolConfig{
		PatrolMolName: "mol-deacon-patrol",
		BeadsDir:      t.TempDir(),
		Assignee:      "deacon",
		ExtraVars:     []string{"idle_effort_threshold=7"},
	})
	if err != nil {
		t.Fatalf("renderPatrolWispDescription: %v", err)
	}
	for _, want := range []string{
		"Mayor's daemon patrol loop.",
		"**Formula Checklist**",
		"gt deacon heartbeat \"starting patrol cycle\"",
		"idle_cycles >= 7",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("description missing %q:\n%s", want, desc)
		}
	}
}

func TestRenderPatrolWispDescription_AppliesOverlay(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	overlayDir := filepath.Join(townRoot, "formula-overlays")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "mol-deacon-patrol.toml"), []byte(`[[step-overrides]]
step_id = "heartbeat"
mode = "append"
description = "overlay heartbeat note"
`), 0644); err != nil {
		t.Fatal(err)
	}

	desc, err := renderPatrolWispDescription(PatrolConfig{
		PatrolMolName: "mol-deacon-patrol",
		BeadsDir:      townRoot,
		Assignee:      "deacon",
	})
	if err != nil {
		t.Fatalf("renderPatrolWispDescription: %v", err)
	}
	if !strings.Contains(desc, "overlay heartbeat note") {
		t.Fatalf("description did not include overlay text:\n%s", desc)
	}
}

func TestRenderPatrolWispDescription_RefinerySubstitutesRigAndEmptyDefaults(t *testing.T) {
	t.Parallel()
	desc, err := renderPatrolWispDescription(PatrolConfig{
		PatrolMolName: "mol-refinery-patrol",
		BeadsDir:      t.TempDir(),
		Assignee:      "gastown/refinery",
	})
	if err != nil {
		t.Fatalf("renderPatrolWispDescription: %v", err)
	}
	if strings.Contains(desc, "{{") {
		t.Fatalf("description contains unresolved placeholder:\n%s", desc)
	}
	if !strings.Contains(desc, "gt agents resolve --role refinery --rig gastown") {
		t.Fatalf("description did not substitute refinery rig:\n%s", desc)
	}
}

func TestRenderPatrolWispDescription_ExtraVarsOverrideRoleVars(t *testing.T) {
	t.Parallel()
	desc, err := renderPatrolWispDescription(PatrolConfig{
		PatrolMolName: constants.MolRefineryPatrol,
		BeadsDir:      t.TempDir(),
		Assignee:      "gastown/refinery",
		ExtraVars:     []string{"rig=override"},
	})
	if err != nil {
		t.Fatalf("renderPatrolWispDescription: %v", err)
	}
	if !strings.Contains(desc, "gt agents resolve --role refinery --rig override") {
		t.Fatalf("description did not use ExtraVars override:\n%s", desc)
	}
	if strings.Contains(desc, "gt agents resolve --role refinery --rig gastown") {
		t.Fatalf("description used role var instead of ExtraVars override:\n%s", desc)
	}
}

func TestUpdatePatrolWispDescriptionUsesBodyFileStdin(t *testing.T) {
	binDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "bd.log")
	writeBDStub(t, binDir, `#!/usr/bin/env sh
{
  printf 'args:%s\n' "$*"
  printf 'stdin:'
  cat
  printf '\n'
} >> "$BD_STUB_LOG"
`, `@echo off
echo args:%* >> %BD_STUB_LOG%
set /p stdin=
echo stdin:%stdin% >> %BD_STUB_LOG%
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logFile)

	err := updatePatrolWispDescription(PatrolConfig{BeadsDir: t.TempDir()}, filepath.Join(t.TempDir(), ".beads"), "gt-wisp-test", "line one\nline two")
	if err != nil {
		t.Fatalf("updatePatrolWispDescription: %v", err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, "args:update gt-wisp-test --body-file=-") {
		t.Fatalf("expected body-file update args, got:\n%s", log)
	}
	if strings.Contains(log, "--description=") {
		t.Fatalf("description must not be passed through argv:\n%s", log)
	}
	if !strings.Contains(log, "stdin:line one\nline two") {
		t.Fatalf("expected multiline description on stdin, got:\n%s", log)
	}
}
