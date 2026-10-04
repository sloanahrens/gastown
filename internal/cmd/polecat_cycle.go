package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/session"
)

// parsePolecatSessionName extracts rig and polecat name from a tmux session name.
// Format: <prefix>-<name> where name is NOT crew-*.
// Returns empty strings and false if the format doesn't match.
//
// Delegates to session.ParseSessionNameWithRegistry, reading rig prefixes from
// reg, for consistent parsing of hyphenated rig names (e.g., gt-my-rig-Toast correctly yields rig="my-rig", name="Toast").
func parsePolecatSessionName(reg *session.PrefixRegistry, sessionName string) (rigName, polecatName string, ok bool) {
	identity, err := session.ParseSessionNameWithRegistry(sessionName, reg)
	if err != nil {
		return "", "", false
	}
	if identity.Role != session.RolePolecat {
		return "", "", false
	}
	if identity.Rig == "" || identity.Name == "" {
		return "", "", false
	}
	return identity.Rig, identity.Name, true
}

// cyclePolecatSession cycles between the polecat sessions of rig.
func cyclePolecatSession(direction int, currentSession, rig string) error {
	allSessions, err := listTmuxSessions()
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	reg := townRegistry()
	var sessions []string
	for _, s := range allSessions {
		if r, _, ok := parsePolecatSessionName(reg, s); ok && r == rig {
			sessions = append(sessions, s)
		}
	}

	return cycleInGroup(direction, currentSession, sessions)
}
