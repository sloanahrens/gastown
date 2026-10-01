// Package runtime provides helpers for runtime-specific integration.
package runtime

import (
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hooks"
	"github.com/steveyegge/gastown/internal/templates/commands"
	"github.com/steveyegge/gastown/internal/workspace"
)

// EnsureSettingsForRole provisions the Claude Code configuration for a role.
// settingsDir is where .claude/settings.json is installed (passed to Claude via
// --settings for crew and polecats; the working directory for the mayor).
// workDir is the agent's working directory where slash commands are provisioned.
//
// Every runtime is the Claude CLI, so every role gets settings and hooks:
// there is no provider that skips them (gt-be0z).
func EnsureSettingsForRole(settingsDir, workDir, role string) error {
	return ensureSettingsForRole(hooks.EnvHome(), settingsDir, workDir, role)
}

func ensureSettingsForRole(home hooks.Home, settingsDir, workDir, role string) error {
	if err := home.InstallForRole(settingsDir, role); err != nil {
		return err
	}

	// Slash commands. Skip provisioning when workDir is nested inside a town
	// root: Claude Code's path-hierarchy traversal already delivers the town
	// root's .claude/commands/ to the agent, so a duplicate copy in workDir
	// causes each command to appear twice.
	if !commandsInherited(workDir) {
		if err := commands.Provision(workDir); err != nil {
			return err
		}
	}

	return nil
}

// commandsInherited reports whether workDir will receive slash commands via
// Claude Code's path-hierarchy traversal without explicit provisioning.
// Commands are inherited when workDir is inside a Gas Town workspace root and
// not separated from it by a nested git repo. Crew and polecat workdirs are
// nested repos, so they still get their own command provisioning.
func commandsInherited(workDir string) bool {
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" || samePath(townRoot, workDir) {
		return false
	}

	gitRoot := gitRootOf(workDir)
	if gitRoot != "" && !samePath(gitRoot, townRoot) {
		return false
	}
	return true
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil {
		return filepath.Clean(absA) == filepath.Clean(absB)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// gitRootOf walks up from dir to find the nearest ancestor directory containing
// a .git entry (file or directory). Returns empty string if none found.
func gitRootOf(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
	}
}

// SessionIDFromEnv returns the runtime session ID, if present.
// It checks GT_SESSION_ID_ENV first, then resolves from the current agent's preset,
// and falls back to CLAUDE_SESSION_ID for backwards compatibility.
func SessionIDFromEnv() string {
	return sessionIDFrom(os.Getenv)
}

// sessionIDFrom is SessionIDFromEnv reading the environment through getenv.
func sessionIDFrom(getenv func(string) string) string {
	if envName := getenv("GT_SESSION_ID_ENV"); envName != "" {
		if sessionID := getenv(envName); sessionID != "" {
			return sessionID
		}
	}
	// Use the current agent's session ID env var from its preset
	if agentName := getenv("GT_AGENT"); agentName != "" {
		townRoot := getenv("GT_ROOT")
		rigPath := ""
		if rig := getenv("GT_RIG"); rig != "" && townRoot != "" {
			rigPath = filepath.Join(townRoot, rig)
		}
		if preset, ok := config.ResolveAgentPreset(agentName, townRoot, rigPath); ok && preset.SessionIDEnv != "" {
			if sessionID := getenv(preset.SessionIDEnv); sessionID != "" {
				return sessionID
			}
		}
	}
	// Backwards-compatible fallback for sessions without GT_AGENT
	return getenv("CLAUDE_SESSION_ID")
}

// StartupNudgeContent returns the work instructions to send as a startup nudge.
func StartupNudgeContent() string {
	return "Check your hook with `" + cli.Name() + " hook`. If work is present, begin immediately."
}
