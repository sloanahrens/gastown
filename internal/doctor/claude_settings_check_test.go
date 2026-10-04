package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestNewClaudeSettingsCheck(t *testing.T) {
	t.Parallel()
	check := NewClaudeSettingsCheck()

	if check.Name() != "claude-settings" {
		t.Errorf("expected name 'claude-settings', got %q", check.Name())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}
}

func TestClaudeSettingsCheck_NoSettingsFiles(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no settings files, got %v", result.Status)
	}
}

// createValidSettings creates a valid settings file with all required elements.
// The filename should be settings.json for valid tests.
func createValidSettings(t *testing.T, path string) {
	t.Helper()

	settings := map[string]any{
		"enabledPlugins": []string{"plugin1"},
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "**",
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "/usr/local/bin/gt prime --hook",
						},
					},
				},
			},
		},
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// createValidPolecatSettings creates a polecat settings file whose Stop hook
// invokes `gt tap polecat-stop-check`, matching the canonical template in
// internal/hooks/config.go DefaultOverrides()["polecats"].
func createValidPolecatSettings(t *testing.T, path string) {
	t.Helper()
	writeSettings(t, path, polecatSettings())
}

// polecatSettings is the canonical polecat settings content, before writing.
// Callers that need a variant (a missing Stop hook, say) mutate the returned
// map first.
func polecatSettings() map[string]any {
	return map[string]any{
		"enabledPlugins": []string{"plugin1"},
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "**",
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "/usr/local/bin/gt prime --hook",
						},
					},
				},
			},
			"Stop": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": `export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" && gt tap polecat-stop-check`,
						},
					},
				},
			},
		},
	}
}

// writeSettings marshals settings to path, creating parent directories.
func writeSettings(t *testing.T, path string, settings map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// createStaleSettings creates a settings file missing required elements.
func createStaleSettings(t *testing.T, path string, missingElements ...string) {
	t.Helper()

	settings := map[string]any{
		"enabledPlugins": []string{"plugin1"},
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "**",
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "/usr/local/bin/gt prime --hook",
						},
					},
				},
			},
		},
	}

	for _, missing := range missingElements {
		switch missing {
		case "enabledPlugins":
			delete(settings, "enabledPlugins")
		case "hooks":
			delete(settings, "hooks")
		case "PATH":
			// Remove prime --hook from SessionStart hooks
			hooks := settings["hooks"].(map[string]any)
			sessionStart := hooks["SessionStart"].([]any)
			hookObj := sessionStart[0].(map[string]any)
			innerHooks := hookObj["hooks"].([]any)
			// Filter out prime --hook command
			var filtered []any
			for _, h := range innerHooks {
				hMap := h.(map[string]any)
				if cmd, ok := hMap["command"].(string); ok && !strings.Contains(cmd, "prime --hook") {
					filtered = append(filtered, h)
				}
			}
			hookObj["hooks"] = filtered
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeSettingsCheck_LeftoverMayorSettingsIgnored(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Leftover settings under the retired mayor role's directory are left on
	// disk: nothing recreates, repairs or deletes them any more.
	createValidSettings(t, filepath.Join(tmpDir, "mayor", ".claude", "settings.json"))
	createValidSettings(t, filepath.Join(tmpDir, "mayor", ".claude", "settings.local.json"))

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for leftover mayor settings, got %v: %s %v", result.Status, result.Message, result.Details)
	}
	if len(check.staleSettings) != 0 {
		t.Errorf("expected no entries for leftover mayor settings, got %+v", check.staleSettings)
	}

	// A fix run must leave the leftover files alone.
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() failed: %v", err)
	}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		if _, err := os.Stat(filepath.Join(tmpDir, "mayor", ".claude", name)); err != nil {
			t.Errorf("leftover mayor %s should be left on disk: %v", name, err)
		}
	}
}

func TestClaudeSettingsCheck_LeftoverDeaconSettingsIgnored(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Leftover settings from the retired deacon role, including a stale
	// settings.local.json that used to be flagged. deacon/ now holds only the
	// dog kennel, so none of it is inspected.
	createValidSettings(t, filepath.Join(tmpDir, "deacon", ".claude", "settings.json"))
	createValidSettings(t, filepath.Join(tmpDir, "deacon", ".claude", "settings.local.json"))

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for leftover deacon settings, got %v: %s %v", result.Status, result.Message, result.Details)
	}
	if len(check.staleSettings) != 0 {
		t.Errorf("expected no entries for leftover deacon settings, got %+v", check.staleSettings)
	}
}

func TestClaudeSettingsCheck_LeftoverWitnessRefineryDirsIgnored(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Leftover witness/ and refinery/ dirs from the retired roles: one with a
	// stale settings.local.json and a workdir settings file, one with no
	// settings at all. Neither may be reported as stale or missing.
	createValidSettings(t, filepath.Join(tmpDir, rigName, "witness", ".claude", "settings.local.json"))
	createValidSettings(t, filepath.Join(tmpDir, rigName, "witness", "rig", ".claude", "settings.json"))
	if err := os.MkdirAll(filepath.Join(tmpDir, rigName, "refinery", "rig"), 0755); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for leftover witness/refinery dirs, got %v: %s %v", result.Status, result.Message, result.Details)
	}
	if len(check.staleSettings) != 0 {
		t.Errorf("expected no entries for leftover witness/refinery dirs, got %+v", check.staleSettings)
	}
}

func TestClaudeSettingsCheck_ValidCrewSettings(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create valid crew settings in correct location (crew/.claude/settings.json)
	// Settings are now shared at the crew parent directory, passed via --settings flag.
	crewSettings := filepath.Join(tmpDir, rigName, "crew", ".claude", "settings.json")
	createValidSettings(t, crewSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for valid crew settings, got %v: %s", result.Status, result.Message)
	}
}

func TestClaudeSettingsCheck_ValidPolecatSettings(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create valid polecat settings in correct location (polecats/.claude/settings.json)
	// with the polecat-specific Stop hook (`gt tap polecat-stop-check`) — see
	// internal/hooks/config.go DefaultOverrides()["polecats"]. The check
	// recognizes role-specific Stop patterns (#3648).
	pcSettings := filepath.Join(tmpDir, rigName, "polecats", ".claude", "settings.json")
	createValidPolecatSettings(t, pcSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for valid polecat settings, got %v: %s", result.Status, result.Message)
	}
}

// TestClaudeSettingsCheck_PolecatStopHookRecognized is the regression test for
// #3648: doctor's claude-settings check used to expect one Stop hook for *all*
// roles, but the polecat hooks template installs `gt tap polecat-stop-check`.
// Result: doctor reported polecat settings as stale, --fix deleted them, the
// daemon recreated the same file, and the check never converged. The fix
// recognizes role-specific Stop patterns; only polecats require one.
func TestClaudeSettingsCheck_PolecatStopHookRecognized(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	pcSettings := filepath.Join(tmpDir, rigName, "polecats", ".claude", "settings.json")
	createValidPolecatSettings(t, pcSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Fatalf("polecat settings with `gt tap polecat-stop-check` should pass; got %v: %s\nDetails: %v",
			result.Status, result.Message, result.Details)
	}

	// A role with no required Stop hook must pass with none.
	crewSettings := filepath.Join(tmpDir, rigName, "crew", ".claude", "settings.json")
	createValidSettings(t, crewSettings)

	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("crew settings with no Stop hook should pass; got %v: %s\nDetails: %v",
			result.Status, result.Message, result.Details)
	}
}

// TestExpectedStopPattern documents the role → expected-Stop-pattern mapping.
// If the canonical hooks template in internal/hooks/config.go changes, this
// test will fail and remind whoever's editing it to keep the doctor check
// in sync.
func TestExpectedStopPattern(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role string
		want string
	}{
		{"polecat", "polecat-stop-check"},
		{"polecats", "polecat-stop-check"}, // both singular and plural in use
		{"crew", ""},
		{"overseer", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := expectedStopPattern(c.role)
		if got != c.want {
			t.Errorf("expectedStopPattern(%q) = %q, want %q", c.role, got, c.want)
		}
	}
}

func TestClaudeSettingsCheck_MissingEnabledPlugins(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Create crew settings.json missing enabledPlugins (content validation)
	crewSettings := filepath.Join(tmpDir, "testrig", "crew", ".claude", "settings.json")
	createStaleSettings(t, crewSettings, "enabledPlugins")

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for missing enabledPlugins, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "1 stale") {
		t.Errorf("expected message about stale settings, got %q", result.Message)
	}
}

func TestClaudeSettingsCheck_MissingHooks(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Create crew settings.json missing hooks entirely (content validation)
	crewSettings := filepath.Join(tmpDir, "testrig", "crew", ".claude", "settings.json")
	createStaleSettings(t, crewSettings, "hooks")

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for missing hooks, got %v", result.Status)
	}
}

func TestClaudeSettingsCheck_MissingSessionStartPrime(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Create crew settings.json missing gt prime in SessionStart (content validation)
	crewSettings := filepath.Join(tmpDir, "testrig", "crew", ".claude", "settings.json")
	createStaleSettings(t, crewSettings, "PATH")

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for missing prime --hook, got %v", result.Status)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "SessionStart hook") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected details to mention SessionStart hook, got %v", result.Details)
	}
}

func TestClaudeSettingsCheck_MissingStopHook(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// A polecat settings.json without its Stop hook is stale; no other role
	// requires one.
	pcSettings := filepath.Join(tmpDir, "testrig", "polecats", ".claude", "settings.json")
	settings := polecatSettings()
	delete(settings["hooks"].(map[string]any), "Stop")
	writeSettings(t, pcSettings, settings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for missing Stop hook, got %v", result.Status)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "Stop hook") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected details to mention Stop hook, got %v", result.Details)
	}
}

func TestClaudeSettingsCheck_WrongLocationCrewParent(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create stale settings.local.json at crew parent dir (old filename, wrong)
	// The correct file is crew/.claude/settings.json
	wrongSettings := filepath.Join(tmpDir, rigName, "crew", ".claude", "settings.local.json")
	createValidSettings(t, wrongSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for wrong location, got %v", result.Status)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "wrong location") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected details to mention wrong location, got %v", result.Details)
	}
}

func TestClaudeSettingsCheck_MultipleStaleFiles(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create multiple stale settings files (all using old settings.local.json which is now stale)
	// settings.local.json is stale - should be settings.json.
	// Each creates BOTH a stale file AND a missing settings.json issue.
	crewWrong := filepath.Join(tmpDir, rigName, "crew", ".claude", "settings.local.json")
	createValidSettings(t, crewWrong) // Valid content but stale filename

	polecatWrong := filepath.Join(tmpDir, rigName, "polecats", ".claude", "settings.local.json")
	createValidSettings(t, polecatWrong) // Valid content but stale filename

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for multiple stale files, got %v", result.Status)
	}
	// 2 stale settings.local.json files + 2 missing settings.json = 4 issues
	// Each directory with stale settings also reports missing correct settings.json
	if !strings.Contains(result.Message, "4") {
		t.Errorf("expected message about 4 issues (2 stale + 2 missing), got %q", result.Message)
	}
}

func TestClaudeSettingsCheck_InvalidJSON(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Create invalid JSON file (settings.json for content validation)
	crewSettings := filepath.Join(tmpDir, "testrig", "crew", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(crewSettings), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crewSettings, []byte("not valid json {"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for invalid JSON, got %v", result.Status)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "invalid JSON") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected details to mention invalid JSON, got %v", result.Details)
	}
}

func TestClaudeSettingsCheck_FixDeletesStaleFile(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Create stale settings.local.json in the crew dir (old filename, now stale)
	staleSettings := filepath.Join(tmpDir, "testrig", "crew", ".claude", "settings.local.json")
	createValidSettings(t, staleSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	// Run to detect - should find stale file AND missing settings.json
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError before fix, got %v", result.Status)
	}

	// Apply fix
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify stale file was deleted
	if _, err := os.Stat(staleSettings); !os.IsNotExist(err) {
		t.Error("expected stale settings.local.json to be deleted")
	}

	// After fix, settings.json is recreated at the correct location by EnsureSettingsForRole.
	// The check should now pass since the correct file exists.
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix (settings recreated at correct location), got %v: %v", result.Status, result.Details)
	}
}

func TestClaudeSettingsCheck_SkipsNonRigDirectories(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Create directories that should be skipped as rigs
	// Note: don't use mayor here because it is a legitimate town-level agent
	// directory - creating subdirs there triggers missing settings detection
	for _, skipDir := range []string{"daemon", ".git", "docs", ".hidden"} {
		dir := filepath.Join(tmpDir, skipDir, "crew", ".claude")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		// These should NOT be detected as rig crew settings
		settingsPath := filepath.Join(dir, "settings.json")
		createStaleSettings(t, settingsPath, "PATH")
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	_ = check.Run(ctx)

	// Count how many stale files were found - should be 0 since none of the
	// skipped directories (daemon, .git, docs, .hidden) are detected as rigs
	if len(check.staleSettings) != 0 {
		t.Errorf("expected 0 stale files (skipped dirs), got %d", len(check.staleSettings))
	}
}

func TestClaudeSettingsCheck_MixedValidAndStale(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create valid mayor settings (settings.json in correct location)
	mayorSettings := filepath.Join(tmpDir, "mayor", ".claude", "settings.json")
	createValidSettings(t, mayorSettings)

	// Create stale crew settings (settings.json missing PATH, in correct location)
	crewSettings := filepath.Join(tmpDir, rigName, "crew", ".claude", "settings.json")
	createStaleSettings(t, crewSettings, "PATH")

	// Create valid polecat settings (settings.json in correct location)
	pcSettings := filepath.Join(tmpDir, rigName, "polecats", ".claude", "settings.json")
	createValidPolecatSettings(t, pcSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for mixed valid/stale, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "1 stale") {
		t.Errorf("expected message about 1 stale file, got %q", result.Message)
	}
	// Should only report the crew settings as stale
	if len(result.Details) != 1 {
		t.Errorf("expected 1 detail, got %d: %v", len(result.Details), result.Details)
	}
}

func TestClaudeSettingsCheck_WrongLocationCrew(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create settings in wrong location (crew/<name>/.claude/ instead of crew/.claude/)
	// Individual member settings are stale - should be shared crew/.claude/settings.json
	wrongSettings := filepath.Join(tmpDir, rigName, "crew", "agent1", ".claude", "settings.local.json")
	createValidSettings(t, wrongSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for wrong location, got %v", result.Status)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "wrong location") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected details to mention wrong location, got %v", result.Details)
	}
}

func TestClaudeSettingsCheck_WrongLocationPolecat(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create settings in wrong location (polecats/<name>/.claude/ instead of polecats/.claude/)
	// Individual polecat intermediate-level settings are stale - should be shared polecats/.claude/settings.json
	wrongSettings := filepath.Join(tmpDir, rigName, "polecats", "pc1", ".claude", "settings.local.json")
	createValidSettings(t, wrongSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for wrong location, got %v", result.Status)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "wrong location") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected details to mention wrong location, got %v", result.Details)
	}
}

// initTestGitRepo makes an empty repository at dir in the gitfake world gf.
func initTestGitRepo(t *testing.T, gf *gitfake.Fake, dir string) {
	t.Helper()
	gf.InitRepo(t, dir)
}

// gitAddAndCommit commits the repository at repoDir as it is on disk, which
// in these tests holds just filePath.
func gitAddAndCommit(t *testing.T, gf *gitfake.Fake, repoDir, filePath string) {
	t.Helper()
	if _, err := filepath.Rel(repoDir, filePath); err != nil {
		t.Fatal(err)
	}
	gf.CommitWorktree(t, repoDir, "Add file")
}

func TestClaudeSettingsCheck_GitStatusUntracked(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create a git repo to simulate a source repo
	rigDir := filepath.Join(tmpDir, rigName, "crew", "max")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	initTestGitRepo(t, gf, rigDir)

	// Create an untracked settings file (not git added)
	wrongSettings := filepath.Join(rigDir, ".claude", "settings.json")
	createValidSettings(t, wrongSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for wrong location, got %v", result.Status)
	}
	// Should mention "untracked"
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "untracked") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected details to mention untracked, got %v", result.Details)
	}
}

func TestClaudeSettingsCheck_GitStatusTrackedClean(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create a git repo to simulate a source repo
	rigDir := filepath.Join(tmpDir, rigName, "crew", "max")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	initTestGitRepo(t, gf, rigDir)

	// Create settings and commit it (tracked, clean)
	trackedSettings := filepath.Join(rigDir, ".claude", "settings.json")
	createValidSettings(t, trackedSettings)
	gitAddAndCommit(t, gf, rigDir, trackedSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	// Tracked settings.json in a worktree is the customer's legitimate project config.
	// It should NOT be flagged as stale or wrong-location.
	// The only issue should be the missing settings.json at crew/.claude/ (informational).
	for _, d := range result.Details {
		if strings.Contains(d, "wrong location") && strings.Contains(d, "settings.json") {
			t.Errorf("tracked settings.json should NOT be flagged as wrong location, got: %s", d)
		}
	}
}

func TestClaudeSettingsCheck_GitStatusTrackedModified(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create a git repo to simulate a source repo
	rigDir := filepath.Join(tmpDir, rigName, "crew", "max")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	initTestGitRepo(t, gf, rigDir)

	// Create settings and commit it
	trackedSettings := filepath.Join(rigDir, ".claude", "settings.json")
	createValidSettings(t, trackedSettings)
	gitAddAndCommit(t, gf, rigDir, trackedSettings)

	// Modify the file after commit
	if err := os.WriteFile(trackedSettings, []byte(`{"modified": true}`), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	// Tracked settings.json (even modified) in a worktree is the customer's project config.
	// It should NOT be flagged as stale or wrong-location.
	for _, d := range result.Details {
		if strings.Contains(d, "wrong location") && strings.Contains(d, "settings.json") {
			t.Errorf("tracked-modified settings.json should NOT be flagged as wrong location, got: %s", d)
		}
	}
}

func TestClaudeSettingsCheck_FixPreservesModifiedFiles(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create a git repo to simulate a source repo
	rigDir := filepath.Join(tmpDir, rigName, "crew", "max")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	initTestGitRepo(t, gf, rigDir)

	// Create settings and commit it
	trackedSettings := filepath.Join(rigDir, ".claude", "settings.json")
	createValidSettings(t, trackedSettings)
	gitAddAndCommit(t, gf, rigDir, trackedSettings)

	// Modify the file after commit
	if err := os.WriteFile(trackedSettings, []byte(`{"modified": true}`), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	// Run to detect and fix
	_ = check.Run(ctx)
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Tracked-modified file should be preserved (customer's project config)
	if _, err := os.Stat(trackedSettings); os.IsNotExist(err) {
		t.Error("expected tracked-modified file to be preserved, but it was deleted")
	}
}

func TestClaudeSettingsCheck_FixDeletesUntrackedFiles(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create a git repo to simulate a source repo
	rigDir := filepath.Join(tmpDir, rigName, "crew", "max")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	initTestGitRepo(t, gf, rigDir)

	// Create an untracked settings file (not git added)
	wrongSettings := filepath.Join(rigDir, ".claude", "settings.json")
	createValidSettings(t, wrongSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	// Run to detect
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError before fix, got %v", result.Status)
	}

	// Apply fix - should delete the untracked file
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify file was deleted
	if _, err := os.Stat(wrongSettings); !os.IsNotExist(err) {
		t.Error("expected untracked file to be deleted")
	}
}

func TestClaudeSettingsCheck_FixPreservesTrackedCleanFiles(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create a git repo to simulate a source repo
	rigDir := filepath.Join(tmpDir, rigName, "crew", "max")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	initTestGitRepo(t, gf, rigDir)

	// Create settings and commit it (tracked, clean) — customer's project config
	trackedSettings := filepath.Join(rigDir, ".claude", "settings.json")
	createValidSettings(t, trackedSettings)
	gitAddAndCommit(t, gf, rigDir, trackedSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	// Run to detect
	_ = check.Run(ctx)

	// Apply fix
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Tracked settings.json should be preserved (customer's project config)
	if _, err := os.Stat(trackedSettings); os.IsNotExist(err) {
		t.Error("expected tracked settings.json to be preserved, but it was deleted")
	}
}

func TestClaudeSettingsCheck_RigRootSettingsFlagged(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create a rig with crew so it's recognised as a rig
	if err := os.MkdirAll(filepath.Join(tmpDir, rigName, "crew"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a rig-root settings.json (legacy pattern)
	rigRootSettings := filepath.Join(tmpDir, rigName, ".claude", "settings.json")
	createValidSettings(t, rigRootSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)
	if result.Status == StatusOK {
		t.Fatal("expected non-OK result when rig-root settings.json exists")
	}

	foundRigRoot := false
	for _, sf := range check.staleSettings {
		if sf.agentType == "rig-root" && sf.path == rigRootSettings {
			foundRigRoot = true
			if !sf.wrongLocation {
				t.Error("expected wrongLocation=true for rig-root settings")
			}
		}
	}
	if !foundRigRoot {
		t.Errorf("expected rig-root stale entry for %s", rigRootSettings)
	}
}

func TestClaudeSettingsCheck_RigRootSettingsFixDeletes(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create a rig with crew so it's recognised as a rig
	if err := os.MkdirAll(filepath.Join(tmpDir, rigName, "crew"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create rig-root settings.json
	rigRootSettings := filepath.Join(tmpDir, rigName, ".claude", "settings.json")
	createValidSettings(t, rigRootSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)
	if result.Status == StatusOK {
		t.Fatal("expected non-OK result before fix")
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Rig-root settings.json should be deleted
	if _, err := os.Stat(rigRootSettings); !os.IsNotExist(err) {
		t.Error("expected rig-root settings.json to be deleted by Fix")
	}
}

// NOTE: TestClaudeSettingsCheck_DetectsStaleCLAUDEmdAtTownRoot and
// TestClaudeSettingsCheck_FixMovesCLAUDEmdToMayor were removed because
// CLAUDE.md at town root is now intentionally created by gt install.
// It serves as the identity anchor for every agent running from the town root.
// See install.go createTownRootAgentMDs() for details.

func TestClaudeSettingsCheck_GitIgnoredFilesNotFlagged(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Initialize git repo at town root
	initTestGitRepo(t, gf, tmpDir)

	// Create .gitignore with CLAUDE.md
	gitignorePath := filepath.Join(tmpDir, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("CLAUDE.md\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitAddAndCommit(t, gf, tmpDir, gitignorePath)

	// Create CLAUDE.md at town root (wrong location but gitignored)
	claudeMdPath := filepath.Join(tmpDir, "CLAUDE.md")
	if err := os.WriteFile(claudeMdPath, []byte("# House Rules\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	// Should pass because the file is properly gitignored
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for gitignored CLAUDE.md, got %v: %s\nDetails: %v",
			result.Status, result.Message, result.Details)
	}
}

func TestClaudeSettingsCheck_TownRootSettingsWarnsInsteadOfKilling(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Create settings.json at town root (wrong location - pollutes all agents)
	staleTownRootDir := filepath.Join(tmpDir, ".claude")
	if err := os.MkdirAll(staleTownRootDir, 0755); err != nil {
		t.Fatal(err)
	}
	staleTownRootSettings := filepath.Join(staleTownRootDir, "settings.json")
	// Create valid settings content
	settingsContent := `{
		"env": {"PATH": "/usr/bin"},
		"enabledPlugins": ["claude-code-expert"],
		"hooks": {
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gt prime"}]}],
			"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "gt handoff"}]}]
		}
	}`
	if err := os.WriteFile(staleTownRootSettings, []byte(settingsContent), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	// Run to detect
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError for town root settings, got %v", result.Status)
	}

	// Verify it's flagged as wrong location
	foundWrongLocation := false
	for _, d := range result.Details {
		if strings.Contains(d, "wrong location") {
			foundWrongLocation = true
			break
		}
	}
	if !foundWrongLocation {
		t.Errorf("expected details to mention wrong location, got %v", result.Details)
	}

	// Apply fix - should NOT return error and should NOT kill sessions
	// (session killing would require tmux which isn't available in tests)
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify stale file was deleted
	if _, err := os.Stat(staleTownRootSettings); !os.IsNotExist(err) {
		t.Error("expected settings.json at town root to be deleted")
	}

	// Verify .claude directory was cleaned up (best-effort)
	if _, err := os.Stat(staleTownRootDir); !os.IsNotExist(err) {
		t.Error("expected .claude directory at town root to be deleted")
	}

	// Nothing recreates town-root settings elsewhere: the file is gone for good.
	if _, err := os.Stat(filepath.Join(tmpDir, "mayor", ".claude")); !os.IsNotExist(err) {
		t.Error("expected no settings recreated under mayor/")
	}
}

// Tests for missing file detection
// When a role directory exists but settings.json is missing, the check should
// report it as a missing file that needs agent restart to create.

func TestClaudeSettingsCheck_MissingSettingsDetails(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create crew directory but NOT the settings.json at crew/.claude/
	crewDir := filepath.Join(tmpDir, rigName, "crew")
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for missing crew settings, got %v", result.Status)
	}

	// Should mention "missing" and "restart"
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "missing") && strings.Contains(d, "restart") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected details to mention missing and restart, got %v", result.Details)
	}

	// Should mention crew agent type
	foundCrew := false
	for _, d := range result.Details {
		if strings.Contains(d, "crew") {
			foundCrew = true
			break
		}
	}
	if !foundCrew {
		t.Errorf("expected details to mention crew, got %v", result.Details)
	}

	// Verify the staleSettings entry has missingFile set to true
	if len(check.staleSettings) != 1 {
		t.Fatalf("expected 1 stale setting, got %d", len(check.staleSettings))
	}
	if !check.staleSettings[0].missingFile {
		t.Error("expected missingFile to be true for missing crew settings")
	}
	if check.staleSettings[0].agentType != "crew" {
		t.Errorf("expected agentType 'crew', got %q", check.staleSettings[0].agentType)
	}
}

func TestClaudeSettingsCheck_MissingCrewSettings(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create crew directory but NOT the shared settings.json at crew/.claude/
	crewDir := filepath.Join(tmpDir, rigName, "crew")
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for missing crew settings, got %v", result.Status)
	}

	// Verify the staleSettings entry has missingFile set to true
	if len(check.staleSettings) != 1 {
		t.Fatalf("expected 1 stale setting, got %d", len(check.staleSettings))
	}
	if !check.staleSettings[0].missingFile {
		t.Error("expected missingFile to be true for missing crew settings")
	}
	if check.staleSettings[0].agentType != "crew" {
		t.Errorf("expected agentType 'crew', got %q", check.staleSettings[0].agentType)
	}
}

func TestClaudeSettingsCheck_MissingPolecatSettings(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create polecats directory but NOT the shared settings.json at polecats/.claude/
	polecatsDir := filepath.Join(tmpDir, rigName, "polecats")
	if err := os.MkdirAll(polecatsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for missing polecat settings, got %v", result.Status)
	}

	// Verify the staleSettings entry has missingFile set to true
	if len(check.staleSettings) != 1 {
		t.Fatalf("expected 1 stale setting, got %d", len(check.staleSettings))
	}
	if !check.staleSettings[0].missingFile {
		t.Error("expected missingFile to be true for missing polecat settings")
	}
	if check.staleSettings[0].agentType != "polecat" {
		t.Errorf("expected agentType 'polecat', got %q", check.staleSettings[0].agentType)
	}
}

func TestClaudeSettingsCheck_MissingMultipleAgentSettings(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create multiple role directories without settings.json at parent level
	dirs := []string{
		filepath.Join(tmpDir, rigName, "polecats"),
		filepath.Join(tmpDir, rigName, "crew"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for missing settings, got %v", result.Status)
	}

	// Should report 2 missing files
	if len(check.staleSettings) != 2 {
		t.Errorf("expected 2 stale settings, got %d", len(check.staleSettings))
	}

	// All should have missingFile set to true
	for _, sf := range check.staleSettings {
		if !sf.missingFile {
			t.Errorf("expected missingFile to be true for %s", sf.path)
		}
	}

	// Message should mention multiple agents
	if !strings.Contains(result.Message, "2") {
		t.Errorf("expected message to mention 2 agents, got %q", result.Message)
	}
}

func TestClaudeSettingsCheck_MixedMissingAndStale(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create polecats with valid settings at correct location
	pcSettings := filepath.Join(tmpDir, rigName, "polecats", ".claude", "settings.json")
	createValidPolecatSettings(t, pcSettings)

	// Create crew directory without settings (missing), plus a stale
	// per-member settings file under it (wrong location).
	crewDir := filepath.Join(tmpDir, rigName, "crew")
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatal(err)
	}
	staleCrewMemberSettings := filepath.Join(crewDir, "max", ".claude", "settings.json")
	createValidSettings(t, staleCrewMemberSettings)

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for mixed issues, got %v", result.Status)
	}

	// Should have 2 issues:
	// 1. crew missing settings.json (reported as missingFile)
	// 2. crew/max stale settings.json (wrongLocation)
	if len(check.staleSettings) != 2 {
		t.Errorf("expected 2 stale settings, got %d: %+v", len(check.staleSettings), check.staleSettings)
	}

	// Verify we have both types
	var hasMissing, hasStale bool
	for _, sf := range check.staleSettings {
		if sf.missingFile {
			hasMissing = true
		}
		if sf.wrongLocation {
			hasStale = true
		}
	}
	if !hasMissing {
		t.Error("expected at least one missing file")
	}
	if !hasStale {
		t.Error("expected at least one stale file")
	}
}

func TestClaudeSettingsCheck_MissingFileOnlyMessage(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create only missing files (no stale files) - crew dir exists but no settings.json
	crewDir := filepath.Join(tmpDir, rigName, "crew")
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for missing settings, got %v", result.Status)
	}

	// When only missing files, message should mention "missing settings"
	if !strings.Contains(result.Message, "missing") {
		t.Errorf("expected message to mention 'missing', got %q", result.Message)
	}

	// Fix hint should mention restart for missing files
	if !strings.Contains(result.FixHint, "gt up --restore") {
		t.Errorf("expected fix hint to mention 'gt up --restore', got %q", result.FixHint)
	}
}

func TestClaudeSettingsCheck_NoMissingFileWhenDirNotExists(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create rig directory structure but NOT the crew/ or polecats/ directories
	// This simulates a rig that doesn't have any agents set up yet
	rigDir := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	result := check.Run(ctx)

	// Should be OK - no settings issues if role directories don't exist
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when role dirs don't exist, got %v: %s", result.Status, result.Message)
	}
}

func TestClaudeSettingsCheck_FixDoesNotDeleteMissingFiles(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create crew directory but NOT the settings.json at crew/.claude/
	crewDir := filepath.Join(tmpDir, rigName, "crew")
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewClaudeSettingsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir}, gf)

	// Run to detect
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError before fix, got %v", result.Status)
	}

	// Apply fix - should NOT try to delete a file that doesn't exist
	// and should NOT error
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed unexpectedly: %v", err)
	}

	// Crew directory should still exist
	if _, err := os.Stat(crewDir); os.IsNotExist(err) {
		t.Error("expected crew directory to still exist after fix")
	}
}
