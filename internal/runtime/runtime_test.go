package runtime

import (
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

type fakeStartupPromptSession struct {
	nudges    []string
	waitCalls int
	waitRC    *config.RuntimeConfig
	waitErr   error
	nudgeErr  error
}

func (f *fakeStartupPromptSession) NudgeSession(_ string, message string) error {
	if f.nudgeErr != nil {
		return f.nudgeErr
	}
	f.nudges = append(f.nudges, message)
	return nil
}

func (f *fakeStartupPromptSession) WaitForRuntimeReady(_ string, rc *config.RuntimeConfig, _ time.Duration) error {
	f.waitCalls++
	f.waitRC = rc
	return f.waitErr
}

func TestSessionIDFromEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no env vars", nil, ""},
		{"CLAUDE_SESSION_ID fallback", map[string]string{"CLAUDE_SESSION_ID": "test-session-123"}, "test-session-123"},
		{"GT_SESSION_ID_ENV names the variable", map[string]string{
			"GT_SESSION_ID_ENV": "CUSTOM_SESSION_ID",
			"CUSTOM_SESSION_ID": "custom-session-456",
			"CLAUDE_SESSION_ID": "claude-session-789",
		}, "custom-session-456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionIDFrom(func(k string) string { return tt.env[k] })
			if got != tt.want {
				t.Errorf("sessionIDFrom(%v) = %q, want %q", tt.env, got, tt.want)
			}
		})
	}
}

func TestEnsureSettingsForRole_ClaudeUsesSettingsDir(t *testing.T) {
	t.Parallel()
	// Claude settings must be installed in settingsDir (passed via --settings flag).
	settingsDir := t.TempDir()
	workDir := t.TempDir()

	err := EnsureSettingsForRole(settingsDir, workDir, "crew")
	if err != nil {
		t.Fatalf("EnsureSettingsForRole() error = %v", err)
	}

	// Settings should be in settingsDir, not workDir
	if _, err := os.Stat(settingsDir + "/.claude/settings.json"); err != nil {
		t.Error("Claude settings should be in settingsDir")
	}
	if _, err := os.Stat(workDir + "/.claude/settings.json"); err == nil {
		t.Error("Claude settings should NOT be in workDir when dirs differ")
	}
}

func TestStartupNudgeContent(t *testing.T) {
	t.Parallel()
	content := StartupNudgeContent()
	if content == "" {
		t.Error("StartupNudgeContent should return non-empty string")
	}
	if !contains(content, "gt hook") {
		t.Error("StartupNudgeContent should mention gt hook")
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && findSubstring(s, substr)
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if s[i+j] != substr[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func makeTownRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(root+"/mayor", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/mayor/town.json", []byte(`{"type":"town"}`), 0644); err != nil {
		t.Fatal(err)
	}
	return root
}

func makeTownRootWithGit(t *testing.T) string {
	t.Helper()
	root := makeTownRoot(t)
	if err := os.MkdirAll(root+"/.git", 0755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCommandsInherited_WorkDirIsNestedInTownRoot(t *testing.T) {
	t.Parallel()
	// workDir is a subdirectory of the town root (same git repo) → inherited
	root := makeTownRootWithGit(t)
	mayorDir := root + "/mayor"

	if !commandsInherited(mayorDir) {
		t.Error("commandsInherited() = false, want true for workDir nested inside town root")
	}
}

func TestCommandsInherited_WorkDirIsTownRoot(t *testing.T) {
	t.Parallel()
	// workDir == git root → not inherited (we're provisioning at the root itself)
	root := makeTownRootWithGit(t)

	if commandsInherited(root) {
		t.Error("commandsInherited() = true, want false when workDir equals the git root")
	}
}

func TestCommandsInherited_WorkDirNestedInTownRootBeforeGitInit(t *testing.T) {
	t.Parallel()
	// gt install creates mayor/deacon settings before it initializes town .git.
	// Those role dirs still inherit town-level commands once install provisions them.
	root := makeTownRoot(t)
	mayorDir := root + "/mayor"

	if !commandsInherited(mayorDir) {
		t.Error("commandsInherited() = false, want true for town role dir before .git exists")
	}
}

func TestCommandsInherited_NestedGitRepoInsideTownRoot(t *testing.T) {
	t.Parallel()
	// Crew/polecat workdirs live in nested git repos under the town root. Claude
	// Code stops at that repo boundary, so they need explicit command provisioning.
	root := makeTownRootWithGit(t)
	workDir := root + "/rig/polecats/chrome/repo"
	if err := os.MkdirAll(workDir+"/.git", 0755); err != nil {
		t.Fatal(err)
	}

	if commandsInherited(workDir) {
		t.Error("commandsInherited() = true, want false for nested git repo inside town root")
	}
}

func TestCommandsInherited_WorkDirIsOutsideTownRoot(t *testing.T) {
	t.Parallel()
	// workDir in a standalone git repo that is NOT a Gas Town workspace → not inherited
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/.git", 0755); err != nil {
		t.Fatal(err)
	}
	subDir := dir + "/src"
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	if commandsInherited(subDir) {
		t.Error("commandsInherited() = true, want false for workDir in non-workspace git repo")
	}
}

func TestCommandsInherited_NoGitRoot(t *testing.T) {
	t.Parallel()
	// workDir has no .git ancestor → not inherited
	dir := t.TempDir()
	// Don't create .git

	if commandsInherited(dir) {
		t.Error("commandsInherited() = true, want false when no .git ancestor found")
	}
}

func TestEnsureSettingsForRole_SkipsCommandsWhenInheritedFromTownRoot(t *testing.T) {
	t.Parallel()
	// Mayor/deacon run inside the town root git repo. Commands provisioned at the
	// town root are inherited by Claude Code's path-hierarchy traversal, so
	// EnsureSettingsForRole must NOT provision a duplicate copy in the role dir.
	root := makeTownRootWithGit(t)
	mayorDir := root + "/mayor"
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := EnsureSettingsForRole(mayorDir, mayorDir, "mayor"); err != nil {
		t.Fatalf("EnsureSettingsForRole() error = %v", err)
	}

	// Commands must NOT be provisioned inside the role dir
	for _, cmd := range []string{"done", "handoff", "review"} {
		path := mayorDir + "/.claude/commands/" + cmd + ".md"
		if _, err := os.Stat(path); err == nil {
			t.Errorf("command %s.md was provisioned in mayor dir, want skipped (would duplicate town-root copy)", cmd)
		}
	}
}

func TestEnsureSettingsForRole_ProvisionCommandsOutsideTownRoot(t *testing.T) {
	t.Parallel()
	// Crew/polecat workDirs are outside the town root git repo.
	// EnsureSettingsForRole must provision commands normally.
	workDir := t.TempDir()
	// workDir has no .git ancestor, so commandsInherited returns false.

	if err := EnsureSettingsForRole(workDir, workDir, "crew"); err != nil {
		t.Fatalf("EnsureSettingsForRole() error = %v", err)
	}

	// At least one command should be provisioned
	provisioned := 0
	for _, cmd := range []string{"done", "handoff", "review"} {
		if _, err := os.Stat(workDir + "/.claude/commands/" + cmd + ".md"); err == nil {
			provisioned++
		}
	}
	if provisioned == 0 {
		t.Error("no commands provisioned in workDir outside town root, want at least one")
	}
}
