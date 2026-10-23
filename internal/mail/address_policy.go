package mail

import (
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
)

// rigSingletonRoles are the roles a rig keeps as a single named workspace
// directly under the rig, addressable as <rig>/<role>.
var rigSingletonRoles = []string{"refinery", "witness"}

// isReservedTownSubpath reports whether address sits under a town-level
// directory that holds no mailboxes of its own: the mayor state directory
// (constants.DirMayor, which the town keeps as its marker), and deacon/<x>,
// which covers the retired deacon/dogs/<name> namespace (gt-29q6g).
func isReservedTownSubpath(address string) bool {
	return strings.HasPrefix(address, constants.DirMayor+"/") ||
		strings.HasPrefix(address, "deacon/")
}

// isRetiredAgentName reports whether name is a role the town no longer hires.
// The retired mayor left its state directory behind (constants.DirMayor, the
// town marker), so the name still exists on disk while the role does not: a
// directory of that name is not a mailbox (gt-rwp7z.5).
func isRetiredAgentName(name string) bool {
	return name == constants.DirMayor
}

// isRetiredAgentAddress reports whether address ends in a retired role name,
// in either the town form ("mayor/") or the rig-scoped form ("gastown/mayor").
func isRetiredAgentAddress(address string) bool {
	trimmed := strings.TrimSuffix(address, "/")
	if trimmed == "" {
		return false
	}
	parts := strings.Split(trimmed, "/")
	return isRetiredAgentName(parts[len(parts)-1])
}

// AgentBeadIDForAddress returns the agent bead an address names, which is the
// bead whose notification_level gates delivery to it. This is the one
// resolver mail and nudge both use, so they agree on which bead an address
// names. prefixForRig maps a rig name to its bead prefix. It returns "" when
// the address names no agent bead (overseer, retired roles, town subpaths, or
// a bare name with no rig).
func AgentBeadIDForAddress(prefixForRig func(rig string) string, address string) string {
	if address == "overseer" {
		return "" // Overseer is a human, no agent bead
	}
	if isReservedTownSubpath(address) || isRetiredAgentAddress(address) {
		return ""
	}

	parts := strings.SplitN(address, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return ""
	}

	rig := parts[0]
	target := parts[1]

	rigPrefix := prefixForRig(rig)

	switch {
	case strings.HasPrefix(target, "crew/"):
		crewName := strings.TrimPrefix(target, "crew/")
		return beads.AgentBeadIDWithPrefix(rigPrefix, rig, constants.RoleCrew, crewName)
	case strings.HasPrefix(target, "polecat/"):
		pcName := strings.TrimPrefix(target, "polecat/")
		return beads.AgentBeadIDWithPrefix(rigPrefix, rig, constants.RolePolecat, pcName)
	case strings.HasPrefix(target, "polecats/"):
		pcName := strings.TrimPrefix(target, "polecats/")
		return beads.AgentBeadIDWithPrefix(rigPrefix, rig, constants.RolePolecat, pcName)
	default:
		// A bare rig/name is a rig singleton (witness, refinery) or a polecat.
		for _, role := range rigSingletonRoles {
			if target == role {
				return beads.AgentBeadIDWithPrefix(rigPrefix, rig, role, "")
			}
		}
		return beads.AgentBeadIDWithPrefix(rigPrefix, rig, constants.RolePolecat, target)
	}
}
