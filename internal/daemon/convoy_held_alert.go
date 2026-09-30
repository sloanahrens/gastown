package daemon

import (
	"fmt"
	"time"
)

// blockedHoldEscalationAfter is how long a bead must stay held on a blocker
// that cannot be resolved or read before the daemon escalates it. The hold is
// the fail-safe ruling in convoy.BlockOf: without an escalation it is a log
// line in the stranded scan's stderr, which the daemon drops, and the bead
// waits forever (gt-gg7w9). A rig store that is down for the length of a Dolt
// restart clears inside this window and never escalates, the same allowance
// storeRecoveryEscalationAfter gives the town store.
const blockedHoldEscalationAfter = 10 * time.Minute

// strandedHold is one bead the stranded scan kept back because a blocker could
// not be resolved or read. It mirrors the cmd package's strandedHold, which
// writes it into `gt convoy stranded --json`.
type strandedHold struct {
	Issue string `json:"issue"`
	// Blocker is "" when the failed read was the bead's own.
	Blocker string `json:"blocker,omitempty"`
	// Cause is "unresolved" or "unreadable"; see convoy.BlockCause.
	Cause  string `json:"cause"`
	Reason string `json:"reason"`
}

// blockedHold is one bead's streak of consecutive scans that held it.
type blockedHold struct {
	since     time.Time
	escalated bool
}

// blockedHoldAlertKey scopes an escalation to one held bead, so `gt escalate`
// records a repeat onto the bead that already represents it.
func blockedHoldAlertKey(issueID string) string {
	return "daemon-convoy:blocked-hold:" + issueID
}

// trackBlockedHolds folds one successful stranded scan into each held bead's
// streak. A bead held on every scan for blockedHoldEscalationAfter is
// escalated once; a bead the scan no longer holds ends its streak and closes
// the escalation if one was raised, so a hold that clears leaves nothing open
// and a later one starts a fresh window. Call with scanMu held, only after a
// scan that returned: a failed scan shows nothing, and ending every streak on
// it would re-escalate each one the next time around.
//
// A convoy the scan skipped with a warning also shows nothing, so its beads
// end their streaks until it reads again. That costs at most a repeat
// escalation, never a missed one.
func (m *ConvoyManager) trackBlockedHolds(stranded []strandedConvoyInfo, now time.Time) {
	if m.blockedHolds == nil {
		m.blockedHolds = make(map[string]*blockedHold)
	}
	seen := make(map[string]bool)
	for _, c := range stranded {
		for _, h := range c.Held {
			if seen[h.Issue] {
				continue // tracked by two convoys: one streak, one escalation
			}
			seen[h.Issue] = true

			streak := m.blockedHolds[h.Issue]
			if streak == nil {
				streak = &blockedHold{since: now}
				m.blockedHolds[h.Issue] = streak
			}
			held := now.Sub(streak.since)
			if streak.escalated || held < blockedHoldEscalationAfter {
				continue
			}
			streak.escalated = true
			if m.escalate != nil {
				m.escalate(blockedHoldAlertKey(h.Issue), "daemon/convoy", blockedHoldMessage(c.ID, h, held))
			}
		}
	}

	for id, streak := range m.blockedHolds {
		if seen[id] {
			continue
		}
		if streak.escalated && m.clearEscalation != nil {
			m.clearEscalation("convoy feed no longer holds the bead", blockedHoldAlertKey(id))
		}
		delete(m.blockedHolds, id)
	}
}

// blockedHoldMessage is the escalation text. gt escalate keeps the first line
// as the title and truncates it, so the bead and the reason, which names the
// blocker and whether it is unresolved or unreadable, come first and the advice
// last.
func blockedHoldMessage(convoyID string, h strandedHold, held time.Duration) string {
	msg := fmt.Sprintf("Convoy feed has held %s for %s: %s (convoy %s).", h.Issue, held.Round(time.Second), h.Reason, convoyID)
	if h.Cause == "unresolved" {
		// Nothing clears this by itself: no store holds the blocker, so the
		// edge is dangling.
		return msg + " Restore the blocker, or drop the edge with bd dep remove."
	}
	return msg + " Check the store named in the reason; the bead is fed again once it reads."
}
