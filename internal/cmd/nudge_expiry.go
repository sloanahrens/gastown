package cmd

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/style"
)

// Install the expiry handler here: internal/mail imports internal/nudge, so the
// queue cannot deliver its own expiry notices, while every delivery plane — the
// UserPromptSubmit hook, the poller, the idle watcher and the ACP propeller —
// runs inside the gt binary, where this init has executed (gt-oexm).
func init() {
	nudge.ExpiryObserver = mailExpiredNudge
}

// mailExpiredNudge delivers a nudge that reached its TTL undelivered to the
// agent it was queued for, so the sender's message outlives its queue entry.
// A session name that does not parse has no agent address to reach, so the
// notice goes to the mayor, who owns town-level routing (gt-oexm).
func mailExpiredNudge(ev nudge.ExpiryEvent) {
	to := expiredNudgeMailTarget(ev.Session)

	router := mail.NewRouter(ev.TownRoot)
	msg := &mail.Message{
		To:      to,
		From:    ev.Nudge.Sender,
		Subject: fmt.Sprintf("EXPIRED NUDGE for %s from %s", ev.Session, ev.Nudge.Sender),
		Body:    formatExpiredNudgeMailBody(ev),
		// A wisp, not a durable mail bead: the expiry notice is an event, and a
		// permanent unowned row per expiry is the pollution gt-vwry describes.
		// The queue's expired/ trace is the durable record of the message.
		Wisp: true,
		// The live delivery already failed or timed out; notifying the session
		// again here would restart the re-injection loop gt-tmlu capped.
		SuppressNotify: true,
	}
	if err := router.Send(msg); err != nil {
		style.PrintWarning("expired nudge for %s could not be mailed to %s: %v", ev.Session, to, err)
	}
}

// expiredNudgeMailTarget picks the mailbox for an expiry notice: the agent
// that owned the session when its name parses, else the mayor, who owns
// town-level routing for a session no rig claims.
func expiredNudgeMailTarget(session string) string {
	if addr := sessionNameToAddress(session); addr != "" {
		return addr
	}
	return constants.RoleMayor
}

// formatExpiredNudgeMailBody reproduces the message and the facts needed to
// judge whether it still matters, so the recipient never has to reconstruct the
// expiry from logs.
func formatExpiredNudgeMailBody(ev nudge.ExpiryEvent) string {
	aged := time.Duration(0)
	if !ev.Nudge.Timestamp.IsZero() {
		aged = ev.Nudge.ExpiredAt.Sub(ev.Nudge.Timestamp).Round(time.Second)
	}

	kept := ev.Trace
	if kept == "" {
		kept = "(not preserved — see stderr of the process that found it)"
	}

	return fmt.Sprintf(`A nudge reached its time-to-live without being delivered to session %s.

  Sender:   %s
  Priority: %s
  Queued:   %s
  Aged:     %s across %d delivery attempt(s)
  Found by: %s
  Kept at:  %s

The message, as queued:

%s
`, ev.Session, ev.Nudge.Sender, ev.Nudge.Priority, ev.Nudge.Timestamp.Format(time.RFC3339),
		aged, ev.Nudge.Attempts, ev.Source, kept, ev.Nudge.Message)
}
