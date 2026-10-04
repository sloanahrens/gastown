package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/nudge"
)

// TestExpiryObserverInstalled guards the wiring that turns a queue expiry into
// a mail notice: the nudge package cannot install its own handler, so a dropped
// init leaves expiries preserved on disk but never delivered (gt-oexm).
func TestExpiryObserverInstalled(t *testing.T) {
	t.Parallel()
	if nudge.ExpiryObserver == nil {
		t.Fatal("nudge.ExpiryObserver is nil; expiry notices would never be mailed")
	}
}

// TestExpiredNudgeMailTargetIsEmptyForUnparseableSession: the session that no
// rig claims has no mailbox to reach, so the notice is dropped rather than
// mailed to a dead address (gt-oexm, mayor retired by gt-rwp7z).
func TestExpiredNudgeMailTargetIsEmptyForUnparseableSession(t *testing.T) {
	t.Parallel()
	if got := expiredNudgeMailTarget(nudgeTestRegistry(), "not a session name"); got != "" {
		t.Errorf("expiredNudgeMailTarget(unparseable session) = %q, want no mailbox", got)
	}
}

// TestMailExpiredNudgeNoMailboxWarnsAndSkips: with no mailbox to reach the
// expiry notice is not mailed at all, and the drop is reported — a silent
// return would leave an operator reading the queue's expired/ trace as the
// only sign that a nudge was lost (gt-oexm, gt-rwp7z).
func TestMailExpiredNudgeNoMailboxWarnsAndSkips(t *testing.T) {
	t.Parallel()
	var warn bytes.Buffer

	mailExpiredNudgeTo(&warn, nudge.ExpiryEvent{
		Session: "not a session name",
		Nudge:   nudge.QueuedNudge{Sender: "witness", Message: "status?"},
	})

	if !strings.Contains(warn.String(), "no reachable mailbox") {
		t.Errorf("warning = %q, want a no-reachable-mailbox warning", warn.String())
	}
}

// TestFormatExpiredNudgeMailBodyCarriesTheMessage checks the notice is readable
// on its own: the recipient must be able to act on it without the expired file
// or the logs of the process that found the expiry (gt-oexm).
func TestFormatExpiredNudgeMailBodyCarriesTheMessage(t *testing.T) {
	t.Parallel()
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
