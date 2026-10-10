// Package beads provides a wrapper for the bd (beads) CLI.
package beads

import (
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// TownBeadsPrefix is the prefix used for town-level agent beads stored in ~/gt/.beads/.
// This distinguishes them from rig-level beads (which use project prefixes like "gt-").
const TownBeadsPrefix = "hq"

// Town-level agent bead IDs use the "hq-" prefix and are stored in town beads.
// These are global agents that operate at the town level (deacon).
//
// The naming convention is:
//   - hq-<role>       for singletons (deacon)
//   - hq-<role>-role  for role definition beads

// NamedRoles are agent roles that include a worker name (rig-level).
var NamedRoles = []string{constants.RoleCrew, constants.RolePolecat}

// isNamedRole checks if a role requires a worker name (rig-level).
func isNamedRole(s string) bool {
	for _, r := range NamedRoles {
		if s == r {
			return true
		}
	}
	return false
}

// ExtractAgentPrefix extracts the prefix from an agent ID.
// Agent IDs have the format: prefix-rig-role-name or prefix-role
// The prefix is always the part before the first hyphen.
// Examples:
//   - "gt-gastown-polecat-nux" -> "gt"
//   - "nx-nexus-polecat-nux" -> "nx"
//   - "gt-deacon" -> "gt"
//   - "bd-beads-crew-max" -> "bd"
func ExtractAgentPrefix(id string) string {
	hyphenIdx := strings.Index(id, "-")
	if hyphenIdx <= 0 {
		return ""
	}
	return id[:hyphenIdx]
}

// ===== Rig-level agent bead ID helpers (gt- prefix) =====

// Agent bead ID naming convention:
//   prefix-rig-role-name
//
// Examples:
//   - gt-deacon (town-level, no rig)
//   - gt-gastown-refinery (rig-level singleton)
//   - gt-gastown-crew-max (rig-level named agent)
//   - gt-gastown-polecat-Toast (rig-level named agent)

// AgentBeadIDWithPrefix generates an agent bead ID using the specified prefix.
// The prefix should NOT include the hyphen (e.g., "gt", "bd", not "gt-", "bd-").
// For town-level agents (deacon), pass empty rig and name.
// For rig-level singletons (refinery), pass empty name.
// For named agents (crew, polecat), pass all three.
//
// When prefix == rig (e.g., rig "ff" with derived prefix "ff"), the rig component
// is omitted to avoid stuttered IDs like "ff-ff-refinery". Instead produces "ff-refinery".
func AgentBeadIDWithPrefix(prefix, rig, role, name string) string {
	if rig == "" || rig == prefix {
		// Town-level agent (rig=="") or collapsed form (rig==prefix):
		//   prefix-role or prefix-role-name
		if name == "" {
			return prefix + "-" + role
		}
		return prefix + "-" + role + "-" + name
	}
	if name == "" {
		// Rig-level singleton: prefix-rig-refinery
		return prefix + "-" + rig + "-" + role
	}
	// Rig-level named agent: prefix-rig-role-name
	return prefix + "-" + rig + "-" + role + "-" + name
}

// AgentBeadID generates the canonical agent bead ID using "gt" prefix.
// For non-gastown rigs, use AgentBeadIDWithPrefix with the rig's configured prefix.
func AgentBeadID(rig, role, name string) string {
	return AgentBeadIDWithPrefix("gt", rig, role, name)
}

// CrewBeadIDWithPrefix returns a Crew worker agent bead ID using the specified prefix.
func CrewBeadIDWithPrefix(prefix, rig, name string) string {
	return AgentBeadIDWithPrefix(prefix, rig, constants.RoleCrew, name)
}

// CrewBeadID returns a Crew worker agent bead ID using "gt" prefix.
func CrewBeadID(rig, name string) string {
	return CrewBeadIDWithPrefix("gt", rig, name)
}

// PolecatBeadIDWithPrefix returns a Polecat agent bead ID using the specified prefix.
func PolecatBeadIDWithPrefix(prefix, rig, name string) string {
	return AgentBeadIDWithPrefix(prefix, rig, constants.RolePolecat, name)
}

// PolecatBeadID returns a Polecat agent bead ID using "gt" prefix.
func PolecatBeadID(rig, name string) string {
	return PolecatBeadIDWithPrefix("gt", rig, name)
}

// AgentBeadIDFor returns the agent bead that role/rig/name owns in the town at
// townRoot: the rig's prefixed bead for a named rig role. It returns "" for an
// identity that owns no bead, which is what every caller's `agentBeadID != ""`
// guard is there to catch.
func AgentBeadIDFor(role, rig, name, townRoot string) string {
	switch role {
	case constants.RolePolecat:
		if rig == "" || name == "" {
			return ""
		}
		return PolecatBeadIDWithPrefix(GetPrefixForRig(townRoot, rig), rig, name)
	case constants.RoleCrew:
		if rig == "" || name == "" {
			return ""
		}
		return CrewBeadIDWithPrefix(GetPrefixForRig(townRoot, rig), rig, name)
	default:
		return ""
	}
}

// ParseAgentBeadID parses an agent bead ID into its components.
// Returns rig, role, name, and whether parsing succeeded.
// For town-level agents, rig will be empty.
// For singletons, name will be empty.
// Accepts any valid prefix (e.g., "gt-", "bd-", "mbca-"), not just "gt-".
//
// The prefix has no upper bound on length: a rig may be registered with a
// four-or-more-character prefix, e.g. "mbca-my_big_cool_app-polecat-nux".
// Anything shorter than two characters is still rejected — the components are
// separated by hyphens, so a one-character token with nothing after it is not
// a prefix.
//
// Handles the collapsed form where prefix == rig (e.g., "ff-polecat-nux" for
// rig "ff"). In collapsed form, the prefix is returned as the rig:
//   - "ff-polecat-nux" → rig="ff", role="polecat", name="nux"
func ParseAgentBeadID(id string) (rig, role, name string, ok bool) {
	// Find the prefix (everything before the first hyphen)
	// Valid prefixes are 2 or more characters (e.g., "gt", "bd", "hq", "mbca")
	hyphenIdx := strings.Index(id, "-")
	if hyphenIdx < 2 {
		return "", "", "", false
	}

	prefix := id[:hyphenIdx]
	rest := id[hyphenIdx+1:]
	parts := strings.Split(rest, "-")

	if len(parts) == 0 {
		return "", "", "", false
	}

	// Single part: town-level role (gt-deacon), or unknown — returned as-is
	// for backward compat.
	if len(parts) == 1 {
		return "", parts[0], "", true
	}

	// Check for collapsed named agent: prefix-role-name (e.g., ff-polecat-nux)
	// This happens when prefix == rig, so the rig component was omitted.
	if isNamedRole(parts[0]) {
		return prefix, parts[0], strings.Join(parts[1:], "-"), true
	}

	// Scan from right for known role markers to handle hyphenated rig names.
	// Format: <rig>-<role>[-<name>] where rig may contain hyphens.
	//
	// When a worker name collides with a role keyword (e.g., a polecat named
	// "crew"), we prefer the named-role interpretation: a named role like
	// "polecat" at position i-1 consumes the keyword at position i as its name.
	for i := len(parts) - 1; i >= 1; i-- {
		p := parts[i]
		if isNamedRole(p) && i < len(parts)-1 {
			// Named roles with a name following: crew, polecat
			return strings.Join(parts[:i], "-"), p, strings.Join(parts[i+1:], "-"), true
		}
		if isNamedRole(p) && i == len(parts)-1 {
			// Named role at the end without a following name. Check if the
			// part to the left is also a named role — if so, this keyword
			// is the worker's name for that role.
			if i >= 2 && isNamedRole(parts[i-1]) {
				return strings.Join(parts[:i-1], "-"), parts[i-1], p, true
			}
			// Named role without a name (invalid but handle gracefully)
			return strings.Join(parts[:i], "-"), p, "", true
		}
	}

	// Fallback: assume 2-part rig/role pattern
	if len(parts) == 2 {
		return parts[0], parts[1], "", true
	}

	return "", "", "", false
}
