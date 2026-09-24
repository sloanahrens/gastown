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

// printSessionNudges drains this session's queued nudges and writes them to
// stderr as a system-reminder block, or nothing when the queue is empty.
//
// It serves the per-cycle touch points a refinery gating MRs back to back
// actually hits — gt mq list, gt mq next, gt mail inbox — which are not
// step boundaries drainSessionNudges already covers (gt-dekkl).
//
// stderr, not stdout: those commands' stdout is machine-read — the refinery
// patrol does MR=$(gt mq next <rig> --quiet), stuck-work-dog pipes
// `gt mq list --json` into jq, boot-triage pipes `gt mail inbox --json` into
// python — so a nudge block on stdout corrupts the parse, and the drain has
// already dropped the nudge from the queue by then, losing it outright.
//
// Best-effort: a no-op outside a workspace, and a drain failure goes to stderr
// rather than failing the command that carried it.
func printSessionNudges() {
	townRoot, err := findMailWorkDir()
	if err != nil {
		return
	}
	fmt.Fprint(os.Stderr, nudge.FormatForInjection(drainSessionNudges(townRoot)))
}
