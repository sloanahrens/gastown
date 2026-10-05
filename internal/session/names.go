// Package session provides polecat session lifecycle management.
package session

import (
	"fmt"
	"strings"
)

// DefaultPrefix is the default beads prefix used when no rig-specific prefix is known.
const DefaultPrefix = "gt"

// HQPrefix is the prefix for town-level sessions (Overseer, Deacon).
const HQPrefix = "hq-"

// CrewSessionName returns the session name for a crew worker in a rig.
// rigPrefix is the rig's beads prefix (e.g., "gt" for gastown, "bd" for beads).
func CrewSessionName(rigPrefix, name string) string {
	return fmt.Sprintf("%s-crew-%s", rigPrefix, name)
}

// PolecatSessionName returns the session name for a polecat in a rig.
// rigPrefix is the rig's beads prefix (e.g., "gt" for gastown, "bd" for beads).
func PolecatSessionName(rigPrefix, name string) string {
	return fmt.Sprintf("%s-%s", rigPrefix, name)
}

// OverseerSessionName returns the session name for the human operator.
// The overseer is the human who controls Gas Town, not an AI agent.
func OverseerSessionName() string {
	return HQPrefix + "overseer"
}

// AssigneeSessionName converts an assignee (rig/name, rig/crew/name or
// rig/polecats/name) to its tmux session name, reading rig prefixes from r.
// persistent is true for a crew identity. An assignee in any other shape has
// no session name.
func (r *PrefixRegistry) AssigneeSessionName(assignee string) (sessionName string, persistent bool) {
	parts := strings.Split(assignee, "/")

	switch len(parts) {
	case 2:
		return PolecatSessionName(r.PrefixForRig(parts[0]), parts[1]), false
	case 3:
		if parts[1] == "crew" {
			return CrewSessionName(r.PrefixForRig(parts[0]), parts[2]), true
		}
		if parts[1] == "polecats" {
			return PolecatSessionName(r.PrefixForRig(parts[0]), parts[2]), false
		}
		return "", false
	default:
		return "", false
	}
}
