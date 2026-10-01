// Package session provides polecat session lifecycle management.
package session

import (
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/tmux"
)

// TownSession represents a town-level tmux session.
type TownSession struct {
	Name      string // Display name (e.g., "Mayor")
	SessionID string // Tmux session ID (e.g., "hq-mayor")
}

// TownSessions returns the list of town-level sessions in shutdown order.
// The Mayor is the only one: the Boot and Deacon sessions were deleted with
// their roles (gt-4k3fj.6.1).
func TownSessions() []TownSession {
	return []TownSession{
		{"Mayor", MayorSessionName()},
	}
}

// WaitForSessionExit polls for a session's process to exit within the given timeout.
// Returns true if the process exited on its own, false if the timeout was reached.
// This allows graceful shutdown (e.g., after Ctrl-C) to actually complete before
// falling through to forceful termination.
func WaitForSessionExit(t *tmux.Tmux, sessionID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running, err := t.HasSession(sessionID)
		if err != nil || !running {
			return true
		}
		time.Sleep(constants.PollInterval)
	}
	return false
}
