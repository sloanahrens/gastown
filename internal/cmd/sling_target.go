package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// spawnPolecatForSling is a seam for tests. Production uses SpawnPolecatForSling.
var spawnPolecatForSling = SpawnPolecatForSling

// resolveTargetAgentFn is a seam for tests. Production uses resolveTargetAgent.
var resolveTargetAgentFn = resolveTargetAgent

// resolveTargetAgent converts a target spec to agent ID, pane, and hook root.
func resolveTargetAgent(target string) (agentID string, pane string, hookRoot string, err error) {
	reg := townRegistry()
	// First resolve to session name
	sessionName, err := resolveRoleToSession(reg, target)
	if err != nil {
		return "", "", "", err
	}

	// Convert session name to agent ID format (this doesn't require tmux)
	agentID = sessionToAgentID(reg, sessionName)

	// Get the pane for that session
	pane, err = getSessionPane(sessionName)
	if err != nil {
		return "", "", "", fmt.Errorf("getting pane for %s: %w", sessionName, err)
	}

	// Get the target's working directory for hook storage
	t := tmux.NewTmux()
	hookRoot, err = t.GetPaneWorkDir(sessionName)
	if err != nil {
		return "", "", "", fmt.Errorf("getting working dir for %s: %w", sessionName, err)
	}

	return agentID, pane, hookRoot, nil
}

// sessionToAgentID converts a session name to agent ID format.
// Uses session.ParseSessionNameWithRegistry for consistent parsing across the codebase.
func sessionToAgentID(reg *session.PrefixRegistry, sessionName string) string {
	identity, err := session.ParseSessionNameWithRegistry(sessionName, reg)
	if err != nil {
		// Fallback for unparseable sessions
		return sessionName
	}
	return canonicalAssigneeAddress(identity)
}

// canonicalAssigneeAddress returns the address used for bead assignees and
// hook-status queries. This matches the form emitted by resolveSelfTarget and
// buildAgentIdentity: the town-level mayor gets a trailing slash.
// session.AgentIdentity.Address() returns the bare name for that role, which
// causes the read/write mismatch in GH#3699.
func canonicalAssigneeAddress(identity *session.AgentIdentity) string {
	addr := identity.Address()
	if identity.Role == session.RoleMayor && !strings.HasSuffix(addr, "/") {
		return addr + "/"
	}
	return addr
}

// resolveSelfTarget determines agent identity, pane, and hook root for slinging to self.
func resolveSelfTarget() (agentID string, pane string, hookRoot string, err error) {
	roleInfo, err := GetRole()
	if err != nil {
		return "", "", "", fmt.Errorf("detecting role: %w", err)
	}

	// Build agent identity from role
	// Town-level agents use trailing slash to match addressToIdentity() normalization
	switch roleInfo.Role {
	case RoleMayor:
		agentID = "mayor/"
	case RolePolecat:
		agentID = fmt.Sprintf("%s/polecats/%s", roleInfo.Rig, roleInfo.Polecat)
	case RoleCrew:
		agentID = fmt.Sprintf("%s/crew/%s", roleInfo.Rig, roleInfo.Polecat)
	default:
		return "", "", "", fmt.Errorf("cannot determine agent identity (role: %s)", roleInfo.Role)
	}

	pane = os.Getenv("TMUX_PANE")
	hookRoot = roleInfo.Home
	if hookRoot == "" {
		// Fallback to git root if home not determined
		hookRoot, err = detectCloneRoot()
		if err != nil {
			return "", "", "", fmt.Errorf("detecting clone root: %w", err)
		}
	}

	return agentID, pane, hookRoot, nil
}

// ResolveTargetOptions controls target resolution behavior.
type ResolveTargetOptions struct {
	DryRun               bool
	Force                bool
	Create               bool
	Account              string
	Agent                string
	NoBoot               bool
	HookBead             string // Bead ID to set atomically during polecat spawn (empty = skip)
	BeadID               string // For cross-rig guard checks (empty = skip guard)
	TownRoot             string
	BaseBranch           string // Override base branch for polecat worktree
	ResumeBranch         string // Existing branch to resume (e.g. PR head); mutually exclusive with BaseBranch
	SkipPolecatAdmission bool   // Caller already holds a capacity reservation
}

// ResolvedTarget holds the results of target resolution.
type ResolvedTarget struct {
	Agent             string
	Pane              string
	WorkDir           string
	HookSetAtomically bool
	NewPolecatInfo    *SpawnedPolecatInfo
	IsSelfSling       bool
}

// resolveTarget resolves a target specification with the running gt's
// collaborators.
func resolveTarget(target string, opts ResolveTargetOptions) (*ResolvedTarget, error) {
	return realSlingDeps().resolveSlingTarget(target, opts)
}

// resolveSlingTarget resolves a target specification to agent, pane, and working directory.
// Handles: "." or empty (self), rig targets (auto-spawn polecat),
// existing agents (with dead polecat fallback).
func (d *slingDeps) resolveSlingTarget(target string, opts ResolveTargetOptions) (*ResolvedTarget, error) {
	result := &ResolvedTarget{}

	// Empty target or "." = self-sling
	if target == "" || target == "." {
		agentID, pane, workDir, err := d.resolveSelf()
		if err != nil {
			if target == "." {
				return nil, fmt.Errorf("resolving self for '.' target: %w", err)
			}
			return nil, err
		}
		result.Agent = agentID
		result.Pane = pane
		result.WorkDir = workDir
		result.IsSelfSling = true
		return result, nil
	}

	// Rig target (auto-spawn polecat)
	if rigName, isRig := d.isRigName(target); isRig {
		// Refuse an e-stopped, parked or docked rig before dispatching
		// (gt-4k3fj.4, gt-4owfd.1, gt-11y).
		townRoot := opts.TownRoot
		if townRoot == "" {
			townRoot = d.cwdTown()
		}
		if townRoot != "" {
			if err := slingBlocked(townRoot, rigName, d.estopOn, d.rigParked); err != nil {
				return nil, err
			}
		}

		if opts.BeadID != "" && !opts.Force {
			if err := d.crossRigGuard(opts.BeadID, rigName+"/polecats/_", opts.TownRoot); err != nil {
				return nil, err
			}
		}
		if opts.BeadID != "" {
			if err := d.verifyInTargetRig(opts.BeadID, rigName, opts.TownRoot); err != nil {
				return nil, err
			}
		}
		if opts.DryRun {
			fmt.Fprintf(d.out, "Would spawn fresh polecat in rig '%s'\n", rigName)
			// peek, not resolve: a dry run prints the route it would take
			// but must not claim a seat. A refusal is printed as the refusal a
			// live sling would raise, since that is the route it would take.
			// The pool is asked whatever --agent says, so the preview matches
			// the live sling (gt-4lbz).
			_, reason, poolErr := d.peekPool(opts.TownRoot, opts.Agent)
			if poolErr != nil {
				fmt.Fprintf(d.out, "  %s\n", poolErr)
			} else if reason != "" {
				fmt.Fprintf(d.out, "  %s\n", reason)
			}
			result.Agent = fmt.Sprintf("%s/polecats/<new>", rigName)
			result.Pane = "<new-pane>"
			return result, nil
		}
		fmt.Fprintf(d.out, "Target is rig '%s', spawning fresh polecat...\n", rigName)
		spawnOpts := SlingSpawnOptions{
			TownRoot:      opts.TownRoot,
			Force:         opts.Force,
			Account:       opts.Account,
			Create:        opts.Create,
			HookBead:      opts.HookBead,
			Agent:         opts.Agent,
			BaseBranch:    opts.BaseBranch,
			ResumeBranch:  opts.ResumeBranch,
			SkipAdmission: opts.SkipPolecatAdmission,
		}
		spawnInfo, err := d.spawnPolecat(rigName, spawnOpts)
		if err != nil {
			return nil, fmt.Errorf("spawning polecat: %w", err)
		}
		result.Agent = spawnInfo.AgentID()
		result.NewPolecatInfo = spawnInfo
		result.WorkDir = spawnInfo.ClonePath
		result.HookSetAtomically = opts.HookBead != ""
		if !opts.NoBoot {
			d.wakeRig(rigName)
		}
		return result, nil
	}

	// Existing agent (with dead polecat fallback).
	// Uses the resolveAgent collaborator — crew, mayor, and all existing agents
	// resolve here, getting their pane for nudge delivery (gt-in7b).
	agentID, pane, workDir, err := d.resolveAgent(target)
	if err != nil {
		if rigName, ok := missingPolecatTargetRigWith(target, opts.Create, opts.TownRoot, d.crewExists); ok {
			if opts.BeadID != "" && !opts.Force {
				if err := d.crossRigGuard(opts.BeadID, rigName+"/polecats/_", opts.TownRoot); err != nil {
					return nil, err
				}
			}
			if opts.BeadID != "" {
				if err := d.verifyInTargetRig(opts.BeadID, rigName, opts.TownRoot); err != nil {
					return nil, err
				}
			}
			polecatName, nameErr := missingPolecatTargetName(target)
			if nameErr != nil {
				return nil, nameErr
			}
			if opts.DryRun {
				// The spawn below is not a preview: it reuses the named
				// polecat for real — detach, reset --hard, clean -f, a new
				// branch, hook_bead — or builds its worktree under --create
				// (gt-hw2gj). The rig branch above guards the same way. The
				// name is fixed here, so no pool peek is needed to print the
				// route.
				//
				// The route is not always reuse: a parked, hooked, busy or
				// dirty named polecat is refused by the reuse gate, and a
				// missing one without --create by the lookup. The peek reads
				// those blockers without taking any, so the preview stops
				// where the sling would (gt-yxc7m). A refusal is terminal, so
				// it is reported as one rather than as a route with a caveat.
				townRoot := opts.TownRoot
				if townRoot == "" {
					townRoot = d.cwdTown()
				}
				refusal := d.peekNamed(townRoot, rigName, SlingSpawnOptions{
					Name:     polecatName,
					HookBead: opts.HookBead,
					Create:   opts.Create,
				})
				if refusal != nil {
					fmt.Fprintf(d.out, "%s Dry run: this sling would be refused.\n", style.Dim.Render("○"))
					fmt.Fprintf(d.out, "%s\n", refusal)
				} else {
					fmt.Fprintf(d.out, "Would reuse/create named polecat %s/%s\n", rigName, polecatName)
				}
				result.Agent = fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
				result.Pane = "<named-pane>"
				return result, nil
			}
			fmt.Fprintf(d.out, "Target polecat %s/%s has no active session; using that polecat (reuse, or create with --create)...\n", rigName, polecatName)
			spawnOpts := SlingSpawnOptions{
				Name:          polecatName,
				TownRoot:      opts.TownRoot,
				Force:         opts.Force,
				Account:       opts.Account,
				Create:        opts.Create,
				HookBead:      opts.HookBead,
				Agent:         opts.Agent,
				BaseBranch:    opts.BaseBranch,
				ResumeBranch:  opts.ResumeBranch,
				SkipAdmission: opts.SkipPolecatAdmission,
			}
			spawnInfo, spawnErr := d.spawnPolecat(rigName, spawnOpts)
			if spawnErr != nil {
				// The named refusal already names the polecat.
				return nil, fmt.Errorf("spawning polecat: %w", spawnErr)
			}
			result.Agent = spawnInfo.AgentID()
			result.NewPolecatInfo = spawnInfo
			result.WorkDir = spawnInfo.ClonePath
			result.HookSetAtomically = opts.HookBead != ""
			if !opts.NoBoot {
				d.wakeRig(rigName)
			}
			return result, nil
		}
		return nil, fmt.Errorf("resolving target: %w", err)
	}
	if opts.BeadID != "" && isPolecatTarget(agentID) {
		parts := strings.Split(agentID, "/")
		if len(parts) >= 3 && parts[1] == "polecats" {
			rigName := parts[0]
			if err := d.verifyInTargetRig(opts.BeadID, rigName, opts.TownRoot); err != nil {
				return nil, err
			}
		}
	}
	result.Agent = agentID
	result.Pane = pane
	result.WorkDir = workDir
	// Detect self-sling by pane: a named target (e.g. "deacon") that resolves to
	// the caller's own tmux pane should not inject the ack prompt — the caller is
	// already running and knows about the hook (GH#3839).
	if pane != "" && pane == d.getenv("TMUX_PANE") {
		result.IsSelfSling = true
	}
	return result, nil
}

// missingPolecatTargetName returns the polecat a target that
// missingPolecatTargetRig accepted names: <rig>/polecats/<name> or the
// <rig>/<name> shorthand. The spawn must use exactly this polecat
// (gt-2w4f9). A malformed name is refused here: an empty one (a trailing
// "polecats/") would otherwise fall through to the pool — the very
// substitution a named target exists to prevent. Character rules for a new
// name are ValidatePoolName's, applied by AddNamedWithOptions under --create;
// an existing polecat is reused under whatever name its directory has.
func missingPolecatTargetName(target string) (string, error) {
	parts := strings.Split(target, "/")
	name := parts[len(parts)-1]
	if isPolecatTarget(target) {
		if len(parts) != 3 {
			return "", fmt.Errorf("invalid polecat target %q: want <rig>/polecats/<name>", target)
		}
		name = parts[2]
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid polecat name %q in target %q; not substituting another polecat", name, target)
	}
	return name, nil
}

func missingPolecatTargetRig(target string, allowShorthand bool, townRoot string) (string, bool) {
	return missingPolecatTargetRigWith(target, allowShorthand, townRoot, crewDirExists)
}

// missingPolecatTargetRigWith is missingPolecatTargetRig asking crewExists
// whether a <rig>/<name> shorthand names a crew member rather than a polecat.
func missingPolecatTargetRigWith(target string, allowShorthand bool, townRoot string, crewExists func(townRoot, rigName, name string) bool) (string, bool) {
	if isPolecatTarget(target) {
		parts := strings.Split(target, "/")
		return parts[0], true
	}
	if !allowShorthand {
		return "", false
	}
	parts := strings.Split(target, "/")
	if len(parts) != 2 || knownRoles[strings.ToLower(parts[1])] {
		return "", false
	}
	if crewExists(townRoot, parts[0], parts[1]) {
		return "", false
	}
	return parts[0], true
}

// crewDirExists reports whether rigName has a crew member called name in the
// town (the cwd's town when townRoot is empty).
func crewDirExists(townRoot, rigName, name string) bool {
	if townRoot == "" {
		townRoot = detectTownRootFromCwd()
	}
	if townRoot == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(townRoot, rigName, "crew", name))
	return err == nil && info.IsDir()
}

// townFromCwd is the cwd's town root, or "" outside a town.
func townFromCwd() string {
	townRoot, _ := workspace.FindFromCwd()
	return townRoot
}
