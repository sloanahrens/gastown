package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/nudge"
)

// TestExpiryObserverInstalled guards the wiring that turns a queue expiry into
// a mail notice: the nudge package cannot install its own handler, so a dropped
// init leaves expiries preserved on disk but never delivered (gt-oexm).
func TestExpiryObserverInstalled(t *testing.T) {
	if nudge.ExpiryObserver == nil {
		t.Fatal("nudge.ExpiryObserver is nil; expiry notices would never be mailed")
	}
}

// TestExpiredNudgeMailTargetFallsBackToMayor covers the session that no rig
// claims: the notice still has a mailbox to reach (gt-oexm).
func TestExpiredNudgeMailTargetFallsBackToMayor(t *testing.T) {
	if got := expiredNudgeMailTarget("not a session name"); got != constants.RoleMayor {
		t.Errorf("expiredNudgeMailTarget(unparseable session) = %q, want %q", got, constants.RoleMayor)
	}
}

// TestFormatExpiredNudgeMailBodyCarriesTheMessage checks the notice is readable
// on its own: the recipient must be able to act on it without the expired file
// or the logs of the process that found the expiry (gt-oexm).
func TestFormatExpiredNudgeMailBodyCarriesTheMessage(t *testing.T) {
	queued := time.Now().Add(-45 * time.Minute)
	ev := nudge.ExpiryEvent{
		TownRoot: t.TempDir(),
		Session:  "gt-gastown-granite",
		Source:   "drain",
		Trace:    "/tmp/nudge_queue/gt-gastown-granite/expired/1-abc.json",
		Nudge: nudge.QueuedNudge{
			Sender:    "witness",
			Message:   "gt-abc is still open — report status",
			Priority:  nudge.PriorityUrgent,
			Timestamp: queued,
			ExpiresAt: queued.Add(30 * time.Minute),
			ExpiredAt: queued.Add(31 * time.Minute),
			Attempts:  2,
		},
	}

	body := formatExpiredNudgeMailBody(ev)

	for _, want := range []string{
		"gt-gastown-granite",
		"witness",
		"gt-abc is still open — report status",
		ev.Trace,
		"2 delivery attempt",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not mention %q:\n%s", want, body)
		}
	}

	// A missing trace must be stated, not rendered as an empty field.
	ev.Trace = ""
	if body := formatExpiredNudgeMailBody(ev); !strings.Contains(body, "not preserved") {
		t.Errorf("body does not report the missing trace:\n%s", body)
	}
}
