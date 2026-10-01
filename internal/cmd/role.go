package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Environment variables for role detection
const (
	EnvGTRole     = "GT_ROLE"
	EnvGTRoleHome = "GT_ROLE_HOME"
)

// RoleInfo contains information about a role and its detection source.
// This is the canonical struct for role detection - used by both GetRole()
// and detectRole() functions.
type RoleInfo struct {
	Role          Role   `json:"role"`
	Source        string `json:"source"` // "env", "cwd", or "explicit"
	Home          string `json:"home"`
	Rig           string `json:"rig,omitempty"`
	Polecat       string `json:"polecat,omitempty"`
	EnvRole       string `json:"env_role,omitempty"`       // Value of GT_ROLE if set
	CwdRole       Role   `json:"cwd_role,omitempty"`       // Role detected from cwd
	Mismatch      bool   `json:"mismatch,omitempty"`       // True if env != cwd detection
	EnvIncomplete bool   `json:"env_incomplete,omitempty"` // True if env was set but missing rig/polecat, filled from cwd
	TownRoot      string `json:"town_root,omitempty"`
	WorkDir       string `json:"work_dir,omitempty"` // Current working directory

	// formulaRun answers the bd cook prime renders formulas with; nil is the
	// bd on PATH. Tests set it so prime never starts bd.
	formulaRun beads.BDRunner
}

// formulaCooker is the bd cook this role's prime renders formulas with.
func (r RoleInfo) formulaCooker() formulaCooker {
	return formulaCooker{run: r.formulaRun}
}

// GetRole returns the current role, checking GT_ROLE first then falling back to cwd.
// This is the canonical function for role detection.
func GetRole() (RoleInfo, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return RoleInfo{}, fmt.Errorf("getting current directory: %w", err)
	}

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return RoleInfo{}, fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return RoleInfo{}, fmt.Errorf("not in a Gas Town workspace")
	}

	return GetRoleWithContext(cwd, townRoot)
}

// GetRoleWithContext returns role info given explicit cwd and town root.
func GetRoleWithContext(cwd, townRoot string) (RoleInfo, error) {
	return getRoleWithContextEnv(cwd, townRoot, os.Getenv)
}

// getRoleWithContextEnv is GetRoleWithContext reading the environment
// through getenv.
func getRoleWithContextEnv(cwd, townRoot string, getenv func(string) string) (RoleInfo, error) {
	info := RoleInfo{
		TownRoot: townRoot,
		WorkDir:  cwd,
	}

	// Check environment variable first
	envRole := getenv(EnvGTRole)
	info.EnvRole = envRole

	// Always detect from cwd for comparison/fallback
	cwdCtx := detectRole(cwd, townRoot)
	info.CwdRole = cwdCtx.Role

	// Determine authoritative role
	if envRole != "" {
		// Parse env role - it might be simple ("mayor") or compound ("gastown/witness")
		parsedRole, rig, polecat := parseRoleString(envRole)
		info.Role = parsedRole
		info.Rig = rig
		info.Polecat = polecat
		info.Source = "env"

		// For simple role strings like "crew" or "polecat", also check
		// GT_RIG and GT_CREW/GT_POLECAT env vars for the full identity
		if info.Rig == "" {
			if envRig := getenv("GT_RIG"); envRig != "" {
				info.Rig = envRig
			}
		}
		if info.Polecat == "" {
			if envCrew := getenv("GT_CREW"); envCrew != "" {
				info.Polecat = envCrew
			} else if envPolecat := getenv("GT_POLECAT"); envPolecat != "" {
				info.Polecat = envPolecat
			}
		}

		// If env is incomplete (missing rig/polecat for roles that need them),
		// fill gaps from cwd detection and mark as incomplete
		needsRig := parsedRole == RolePolecat || parsedRole == RoleCrew
		needsPolecat := parsedRole == RolePolecat || parsedRole == RoleCrew

		if needsRig && info.Rig == "" && cwdCtx.Rig != "" {
			info.Rig = cwdCtx.Rig
			info.EnvIncomplete = true
		}
		if needsPolecat && info.Polecat == "" && cwdCtx.Polecat != "" {
			info.Polecat = cwdCtx.Polecat
			info.EnvIncomplete = true
		}

		// Check for mismatch with cwd detection
		if cwdCtx.Role != RoleUnknown && cwdCtx.Role != parsedRole {
			info.Mismatch = true
		}
	} else {
		// Fall back to cwd detection - copy all fields from cwdCtx
		info.Role = cwdCtx.Role
		info.Rig = cwdCtx.Rig
		info.Polecat = cwdCtx.Polecat
		info.Source = "cwd"
	}

	// Determine home directory
	info.Home = getRoleHome(info.Role, info.Rig, info.Polecat, townRoot)

	return info, nil
}

// detectRole detects the agent role from the current working directory path.
// This is the cwd-based fallback used by GetRoleWithContext when GT_ROLE is not set.
func detectRole(cwd, townRoot string) RoleInfo {
	ctx := RoleInfo{
		Role:     RoleUnknown,
		TownRoot: townRoot,
		WorkDir:  cwd,
		Source:   "cwd",
	}

	// Get relative path from town root
	relPath, err := filepath.Rel(townRoot, cwd)
	if err != nil {
		return ctx
	}

	// Normalize and split path
	relPath = filepath.ToSlash(relPath)
	parts := strings.Split(relPath, "/")

	// Town root is a neutral location — don't infer any role from it.
	// The mayor's actual home is mayor/ (matched below).
	if relPath == "." || relPath == "" {
		return ctx
	}

	// Check for mayor role: mayor/ or mayor/rig/
	if len(parts) >= 1 && parts[0] == "mayor" {
		ctx.Role = RoleMayor
		return ctx
	}

	// deacon/ held the deleted deacon, boot and dog roles (gt-4k3fj.6.1,
	// gt-ckunw). It is not a rig and has no role.
	if len(parts) >= 1 && parts[0] == "deacon" {
		return ctx
	}

	// At this point, first part should be a rig name
	if len(parts) < 1 {
		return ctx
	}
	rigName := parts[0]
	ctx.Rig = rigName

	// Check for mayor: <rig>/mayor/ or <rig>/mayor/rig/
	if len(parts) >= 2 && parts[1] == "mayor" {
		ctx.Role = RoleMayor
		return ctx
	}

	// <rig>/witness/ was the retired witness's home (gt-4k3fj.6.1): no role.
	if len(parts) >= 2 && parts[1] == "witness" {
		return ctx
	}

	// Check for polecat: <rig>/polecats/<name>/
	if len(parts) >= 3 && parts[1] == "polecats" {
		ctx.Role = RolePolecat
		ctx.Polecat = parts[2]
		return ctx
	}

	// Check for crew: <rig>/crew/<name>/
	if len(parts) >= 3 && parts[1] == "crew" {
		ctx.Role = RoleCrew
		ctx.Polecat = parts[2] // Use Polecat field for crew member name
		return ctx
	}

	// Default: could be rig root - treat as unknown
	return ctx
}

// parseRoleString parses a role string like "mayor", "gastown/crew/max", or "gastown/polecats/alpha".
func parseRoleString(s string) (Role, string, string) {
	s = strings.TrimSpace(s)

	// Normalize consecutive slashes (e.g. "gamestore//refinery" → "gamestore/refinery")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	s = strings.TrimSuffix(s, "/")

	// Simple roles
	switch s {
	case constants.RoleMayor:
		return RoleMayor, "", ""
	}

	// Compound roles: rig/role or rig/polecats/name or rig/crew/name
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		// Unknown format, try to match as simple role
		return Role(s), "", ""
	}

	rig := parts[0]

	switch parts[1] {
	case "boot", "witness", "refinery":
		// The boot, witness (gt-4k3fj.6.1) and refinery (gt-v4ssj.6) roles
		// were deleted. A stale GT_ROLE of deacon/boot, <rig>/witness or
		// <rig>/refinery is an unknown role, not a polecat of that name (a
		// name the pool already reserves).
		return Role(s), "", ""
	case "polecats":
		if len(parts) >= 3 {
			return RolePolecat, rig, parts[2]
		}
		return RolePolecat, rig, ""
	case constants.RoleCrew:
		if len(parts) >= 3 {
			return RoleCrew, rig, parts[2]
		}
		return RoleCrew, rig, ""
	default:
		// Might be rig/polecatName format
		return RolePolecat, rig, parts[1]
	}
}

// ActorString returns the actor identity string for beads attribution.
// Format matches beads created_by convention:
//   - Simple roles: "mayor"
//   - Workers: "gastown/crew/max", "gastown/polecats/Toast"
func (info RoleInfo) ActorString() string {
	switch info.Role {
	case RoleMayor:
		return "mayor"
	case RolePolecat:
		if info.Rig != "" && info.Polecat != "" {
			return fmt.Sprintf("%s/polecats/%s", info.Rig, info.Polecat)
		}
		return "polecat"
	case RoleCrew:
		if info.Rig != "" && info.Polecat != "" {
			return fmt.Sprintf("%s/crew/%s", info.Rig, info.Polecat)
		}
		return "crew"
	default:
		return string(info.Role)
	}
}

// getRoleHome returns the canonical home directory for a role.
func getRoleHome(role Role, rig, polecat, townRoot string) string {
	switch role {
	case RoleMayor:
		return filepath.Join(townRoot, "mayor")
	case RolePolecat:
		if rig == "" || polecat == "" {
			return ""
		}
		return filepath.Join(townRoot, rig, "polecats", polecat)
	case RoleCrew:
		if rig == "" || polecat == "" {
			return ""
		}
		return filepath.Join(townRoot, rig, "crew", polecat)
	default:
		return ""
	}
}
