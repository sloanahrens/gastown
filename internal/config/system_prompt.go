package config

import (
	"fmt"
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
// refinery, mayor, deacon) share one file per rig or town. Polecat, crew and dog
// templates interpolate the agent's own name and worktree, so those roles get
// one file per agent (system-prompt-<name>.md) and "" when the name is unknown.
// Dog files live in the dog's own kennel (deacon/dogs/<name>/.claude), which is
// also where its hooks settings file is written.
// Returns "" for roles that do not use a system-prompt file (boot: its prime
// already fits the hook budget) or when the scope path is missing.
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
	case constants.RoleDog:
		if townRoot == "" || agentName == "" {
			return ""
		}
		dir = filepath.Join(townRoot, "deacon", "dogs", agentName)
		name = "system-prompt-" + agentName + ".md"
	default:
		return ""
	}
	return filepath.Join(dir, ".claude", name)
}

// SystemPromptRenderer renders the static role text for role/agentName into
// path. internal/cmd installs the real renderer at init (the role templates
// and their rig/session helpers import this package, so config cannot call
// them directly). nil means "no renderer": the flag is then added only when
// the file already exists, and gt prime prints the static text itself.
var SystemPromptRenderer func(role, townRoot, rigPath, agentName, path string) error

// withRoleSystemPromptFlag appends --append-system-prompt-file <path> and sets
// GT_SYSTEM_PROMPT_FILE for Claude agents once the role's rendered system
// prompt file exists. A missing file is rendered first through
// SystemPromptRenderer, so the FIRST spawn of a polecat name is bounded like
// every later one; before this, gt prime wrote the file "for the next spawn"
// and the first session got the full ~26k-char prime, which the Claude Code
// hook budget truncates to a 2 KB preview (gt-t30p). When no renderer is
// installed or rendering fails, the config is returned unchanged and gt prime
// prints the static role text itself, so it degrades to the old behavior
// rather than a dead session.
func withRoleSystemPromptFlag(rc *RuntimeConfig, role, townRoot, rigPath, agentName string) *RuntimeConfig {
	if rc == nil || !isClaudeAgent(rc) {
		return rc
	}
	path := SystemPromptFilePath(role, townRoot, rigPath, agentName)
	if path == "" {
		return rc
	}
	if !systemPromptFileReady(path) {
		if SystemPromptRenderer == nil {
			return rc
		}
		if err := SystemPromptRenderer(role, townRoot, rigPath, agentName, path); err != nil {
			// Degrade loudly: the session still starts (gt prime prints the
			// static text), but a broken template must not hide here.
			fmt.Fprintf(os.Stderr, "gt: system prompt for %s not rendered (%v); the first prime will carry it\n", role, err)
			return rc
		}
		if !systemPromptFileReady(path) {
			return rc
		}
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

// ResolveRoleAgentConfigWithOverride resolves agentOverride for role (falling
// back to role_agents when agentOverride is empty) and applies the role-level
// Claude flags that every spawn path must carry: --settings when the role's
// settings dir differs from its working dir, and --append-system-prompt-file
// when the role's rendered system prompt exists. agentName is the polecat or
// crew worker name; other roles pass "".
//
// Spawn paths that resolve an explicit agent (GT_AGENT on handoff, --agent on
// sling) used to call ResolveAgentConfigWithOverride directly and so skipped
// both flags, which left self-handoff respawns without the system prompt file.
func ResolveRoleAgentConfigWithOverride(role, townRoot, rigPath, agentOverride, agentName string) (*RuntimeConfig, error) {
	var rc *RuntimeConfig
	if agentOverride == "" {
		rc = ResolveRoleAgentConfig(role, townRoot, rigPath)
	} else {
		var err error
		rc, _, err = ResolveAgentConfigWithOverride(townRoot, rigPath, agentOverride)
		if err != nil {
			return nil, err
		}
	}
	rc = withRoleSettingsFlag(rc, role, rigPath)
	return withRoleSystemPromptFlag(rc, role, townRoot, rigPath, agentName), nil
}

// systemPromptFileReady reports whether a rendered system prompt exists at path.
func systemPromptFileReady(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
