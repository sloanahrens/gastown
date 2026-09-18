package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// The role templates render through this package (they need rig, session and
// workspace helpers that import config), so config cannot render the system
// prompt file itself. Install the renderer here; every spawn path runs inside
// the gt binary, where this init has executed.
func init() {
	config.SystemPromptRenderer = renderSystemPromptFileForSpawn
}

// errNoSystemPromptForRole reports a role/agent combination that has no
// system prompt file (dogs, boot, an unknown role, a polecat without a name).
var errNoSystemPromptForRole = errors.New("role has no system prompt file")

// renderSystemPromptFileForSpawn writes the static role text for role/agentName
// to path before the agent starts, so the runtime can pass it back with
// --append-system-prompt-file on the very first spawn (gt-t30p). It renders
// exactly what gt prime would render inside the session, including the
// working directory that the polecat template prints, so gt prime's refresh
// on the first run finds the file already current.
func renderSystemPromptFileForSpawn(role, townRoot, rigPath, agentName, path string) error {
	if path == "" {
		return errNoSystemPromptForRole
	}
	ctx, err := spawnRoleContext(role, townRoot, rigPath, agentName)
	if err != nil {
		return err
	}
	text, fromTemplate, err := staticRoleText(ctx)
	if err != nil {
		return err
	}
	if !fromTemplate {
		// Only template-rendered text is worth persisting (gt prime applies
		// the same rule); the hardcoded fallback stays in the hook output.
		return errors.New("role template unavailable")
	}
	_, err = writeSystemPromptFile(path, text)
	return err
}

// spawnRoleContext rebuilds the RoleContext gt prime derives from GT_ROLE and
// the session cwd, for an agent that has not started yet. WorkDir follows the
// directory each role's session is launched in: the polecat worktree
// (polecats/<name>/<rig>, or the legacy polecats/<name> when the nested clone
// does not exist), crew/<name>, witness, refinery/rig, mayor and deacon.
func spawnRoleContext(role, townRoot, rigPath, agentName string) (RoleContext, error) {
	if townRoot == "" {
		return RoleContext{}, errors.New("town root is required to render a system prompt")
	}
	var r Role
	rigName := ""
	if rigPath != "" {
		rigName = filepath.Base(rigPath)
	}
	switch role {
	case constants.RoleMayor:
		r = RoleMayor
	case constants.RoleDeacon:
		r = RoleDeacon
	case constants.RoleWitness:
		r = RoleWitness
	case constants.RoleRefinery:
		r = RoleRefinery
	case constants.RolePolecat:
		r = RolePolecat
	case constants.RoleCrew:
		r = RoleCrew
	default:
		return RoleContext{}, fmt.Errorf("%w: %q", errNoSystemPromptForRole, role)
	}
	switch r {
	case RoleWitness, RoleRefinery:
		if rigName == "" {
			return RoleContext{}, fmt.Errorf("%s needs a rig path", role)
		}
	case RolePolecat, RoleCrew:
		if rigName == "" || agentName == "" {
			return RoleContext{}, fmt.Errorf("%s needs a rig path and an agent name", role)
		}
	default:
		rigName = ""
	}
	workDir := getRoleHome(r, rigName, agentName, townRoot)
	if r == RolePolecat {
		// Sessions start in the nested clone when it exists (polecat.Manager.clonePath).
		if nested := filepath.Join(workDir, rigName); isDir(nested) {
			workDir = nested
		}
	}
	return RoleContext{
		Role:     r,
		Rig:      rigName,
		Polecat:  agentName,
		TownRoot: townRoot,
		WorkDir:  workDir,
	}, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
