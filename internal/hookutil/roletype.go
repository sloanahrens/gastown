// Package hookutil provides shared utilities for agent hook installers.
package hookutil

import "github.com/steveyegge/gastown/internal/constants"

// IsAutonomousRole returns true if the given role operates without human
// prompting and needs automatic mail injection on startup.
//
// Autonomous roles: polecat.
// Interactive roles: mayor, crew (and anything else).
//
// This is the single source of truth for the autonomous/interactive
// classification used by the hook installer (settings-autonomous.json vs
// settings-interactive.json).
func IsAutonomousRole(role string) bool {
	switch role {
	case constants.RolePolecat:
		return true
	default:
		return false
	}
}
