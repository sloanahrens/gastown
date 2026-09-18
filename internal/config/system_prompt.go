package config

import (
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/constants"
)

// EnvSystemPromptFile names the rendered role system-prompt file that the agent
// runtime was started with (--append-system-prompt-file). gt prime omits the
// static role text from its hook output when this is set and the file exists,
// because the model already has that text in its system prompt.
const EnvSystemPromptFile = "GT_SYSTEM_PROMPT_FILE"

// SystemPromptFileName is the file gt prime renders the static role text into,
// next to the role's hooks settings file.
const SystemPromptFileName = "system-prompt.md"

// SystemPromptFilePath returns where the static role text for an agent lives,
// always under the role's .claude directory next to the Claude hooks settings
// file (only Claude agents receive the flag). Singleton roles (witness,
// refinery, mayor, deacon) share one file per rig or town. Polecat and crew
// templates interpolate the agent's own name and worktree, so those roles get
// one file per agent (system-prompt-<name>.md) and "" when the name is unknown.
// Returns "" for roles that do not use a system-prompt file (dog, boot: their
// prime already fits the hook budget) or when the scope path is missing.
func SystemPromptFilePath(role, townRoot, rigPath, agentName string) string {
	var dir string
	name := SystemPromptFileName
	switch role {
	case constants.RolePolecat, constants.RoleCrew:
		if rigPath == "" || agentName == "" {
			return ""
		}
		dir = RoleSettingsDir(role, rigPath)
		name = "system-prompt-" + agentName + ".md"
	case constants.RoleWitness, constants.RoleRefinery:
		if rigPath == "" {
			return ""
		}
		dir = RoleSettingsDir(role, rigPath)
	case constants.RoleMayor, constants.RoleDeacon:
		if townRoot == "" {
			return ""
		}
		dir = filepath.Join(townRoot, role)
	default:
		return ""
	}
	return filepath.Join(dir, ".claude", name)
}

// withRoleSystemPromptFlag appends --append-system-prompt-file <path> and sets
// GT_SYSTEM_PROMPT_FILE for Claude agents when the role's rendered system
// prompt file exists. When the file is missing (first spawn after install,
// non-Claude runtime) the config is returned unchanged and gt prime prints the
// static role text itself, so a missing file degrades to today's behavior
// rather than a dead session.
func withRoleSystemPromptFlag(rc *RuntimeConfig, role, townRoot, rigPath, agentName string) *RuntimeConfig {
	if rc == nil || !isClaudeAgent(rc) {
		return rc
	}
	path := SystemPromptFilePath(role, townRoot, rigPath, agentName)
	if path == "" {
		return rc
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return rc
	}
	for _, arg := range rc.Args {
		if arg == "--append-system-prompt-file" {
			return rc
		}
	}
	rc.Args = append(rc.Args, "--append-system-prompt-file", path)
	// Copy before mutating: rc.Env may be shared with cached settings.
	env := make(map[string]string, len(rc.Env)+1)
	for k, v := range rc.Env {
		env[k] = v
	}
	env[EnvSystemPromptFile] = path
	rc.Env = env
	return rc
}
