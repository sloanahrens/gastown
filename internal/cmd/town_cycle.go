package cmd

import (
	"fmt"
)

// getTownLevelSessions returns the town-level session names for the current workspace.
func getTownLevelSessions() []string {
	return []string{getMayorSessionName()}
}

// isTownLevelSession checks if the given session name is a town-level session.
// Town-level sessions (the Mayor) use the "hq-" prefix, so we can identify
// them by name alone without requiring workspace context. This is critical for
// tmux run-shell which may execute from outside the workspace directory.
func isTownLevelSession(sessionName string) bool {
	// Town-level sessions are identified by their fixed names
	return sessionName == getMayorSessionName() // "hq-mayor"
}

// cycleTownSession switches to the next or previous town-level session.
// direction: 1 for next, -1 for previous
// sessionOverride: if non-empty, use this instead of detecting current session
func cycleTownSession(direction int, sessionOverride string) error {
	currentSession, err := resolveCurrentSession(sessionOverride)
	if err != nil {
		return fmt.Errorf("not in a tmux session: %w", err)
	}
	if currentSession == "" {
		return fmt.Errorf("not in a tmux session")
	}

	if !isTownLevelSession(currentSession) {
		return nil
	}

	sessions, err := findRunningTownSessions()
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	return cycleInGroup(direction, currentSession, sessions)
}

// findRunningTownSessions returns a list of currently running town-level sessions.
func findRunningTownSessions() ([]string, error) {
	allSessions, err := listTmuxSessions()
	if err != nil {
		return nil, fmt.Errorf("listing tmux sessions: %w", err)
	}

	townLevelSessions := getTownLevelSessions()
	if townLevelSessions == nil {
		return nil, fmt.Errorf("cannot determine town-level sessions")
	}

	var running []string
	for _, s := range allSessions {
		for _, townSession := range townLevelSessions {
			if s == townSession {
				running = append(running, s)
				break
			}
		}
	}

	return running, nil
}
