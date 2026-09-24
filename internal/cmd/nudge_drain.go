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

// nudgeInjectionOutput drains this session's queued nudges and formats them
// for terminal injection, or "" if there is nothing to show. Callers just
// fmt.Print the result — printing an empty string is a no-op.
//
// gt-saz7a wired this into await-signal, await-event, and patrol report, the
// step boundaries a long-running patrol turn actually passes through. But a
// refinery that stays busy gating MRs back to back never reaches any of
// those — its real per-cycle touch points are gt mq list, gt mq next, and gt
// mail inbox, called directly from the patrol loop (gt-dekkl).
func nudgeInjectionOutput(townRoot string) string {
	return nudge.FormatForInjection(drainSessionNudges(townRoot))
}
