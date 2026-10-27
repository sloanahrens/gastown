package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

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
// file (only Claude agents receive the flag). Polecat and crew templates
// interpolate the agent's own name and worktree, so those roles get one file
// per agent (system-prompt-<name>.md) and "" when the name is unknown. Returns
// "" for roles that do not use a system-prompt file or when the scope path is
// missing.
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
	default:
		return ""
	}
	return filepath.Join(dir, ".claude", name)
}

// SystemPromptTarget says where a rendered system prompt will run. The zero value is the host: the
// file is the role template's text as it renders. A sandbox target is the sandbox's final bytes: the
// host town root is rewritten to SandboxTownRoot and Note is appended, so the host renders the bytes
// the sandbox reads and the upload copies them verbatim (gt-uvwi6).
type SystemPromptTarget struct {
	Sandbox         bool
	SandboxTownRoot string
	Note            string
}

// SystemPromptRender renders the static role text for role/agentName into path, for target.
type SystemPromptRender func(role, townRoot, rigPath, agentName, path string, target SystemPromptTarget) error

// SystemPromptRenderer renders the static role text for role/agentName into
// path. internal/cmd installs the real renderer at init (the role templates
// and their rig/session helpers import this package, so config cannot call
// them directly). nil means "no renderer": the flag is then added only when
// the file already exists, and gt prime prints the static text itself.
var SystemPromptRenderer SystemPromptRender

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
func withRoleSystemPromptFlag(reg *AgentRegistry, rc *RuntimeConfig, role, townRoot, rigPath, agentName string) *RuntimeConfig {
	if rc == nil {
		return rc
	}
	path := SystemPromptFilePath(role, townRoot, rigPath, agentName)
	if path == "" {
		return rc
	}
	// Render on every spawn, not only when the file is missing: a file left by
	// an earlier spawn holds that day's directives, and the renderer rewrites it
	// only when the content differs (gt-zqj9t).
	w := reg.host().warnWriter()
	if render := reg.host().systemPromptRenderer(); render != nil {
		if err := render(role, townRoot, rigPath, agentName, path, SystemPromptTarget{}); err != nil {
			if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
				// Degrade loudly: the spawn runs on the file it has, and the
				// warning says how old that file is.
				age := time.Since(info.ModTime()).Round(time.Minute)
				fmt.Fprintf(w, "gt: system prompt for %s not re-rendered (%v); using %s, written %s ago\n", role, err, path, age)
			} else {
				// Degrade loudly: the session still starts (gt prime prints the
				// static text), but a broken template must not hide here.
				fmt.Fprintf(w, "gt: system prompt for %s not rendered (%v); the first prime will carry it\n", role, err)
				return rc
			}
		}
	}
	if !systemPromptFileReady(path) {
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
	// A polecat or crew session takes its directives from the system-prompt file,
	// so a directive source that cannot be read stops the spawn here (gt-qwcdz).
	// Degrading to a start without the file would run the agent with none of
	// its operator policy, and nothing downstream would stop it.
	if role == constants.RolePolecat || role == constants.RoleCrew {
		rigName := ""
		if rigPath != "" {
			rigName = filepath.Base(rigPath)
		}
		if _, err := ReadRoleDirectives(role, townRoot, rigName); err != nil {
			return nil, fmt.Errorf("%s: directives unreadable, spawn refused: %w", role, err)
		}
	}
	reg := AgentRegistryFor(townRoot, rigPath)
	rc = withRoleSettingsFlag(rc, role, rigPath)
	return withRoleSystemPromptFlag(reg, rc, role, townRoot, rigPath, agentName), nil
}

// systemPromptFileReady reports whether a rendered system prompt exists at path.
func systemPromptFileReady(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
