package cmd

import (
	"fmt"
	"os"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
)

// drainSessionNudges drains queued nudges for the current tmux session,
// returning them in FIFO order (or nil if there are none). Drain errors are
// logged to stderr rather than returned — a drain failure must never block
// the caller's own work.
//
// This is the delivery path for an agent that never fully idles: the
// UserPromptSubmit hook (gt mail check --inject) only fires at the next turn
// boundary, and a patrol stuck mid-gate for hours never reaches one. Commands
// that run every patrol cycle regardless of gate duration — await-signal and
// patrol report — are step boundaries the loop actually passes through, so
// draining here surfaces nudges as ordinary tool output instead of waiting
// for idle (gt-saz7a).
func drainSessionNudges(townRoot string) []nudge.QueuedNudge {
	sessionName := tmux.CurrentSessionName()
	if sessionName == "" {
		return nil
	}
	drained, err := nudge.Drain(townRoot, sessionName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nudge queue drain error: %v\n", err)
		return nil
	}
	return drained
}
