package daemon

import (
	"fmt"
	"strings"
)

// unnamedEscalationKeyPrefix is the alert key of the escalation filed for a seat that stopped on
// ESCALATED without filing one: one key per bead, so a second such exit on the same bead repeats
// the key and gt escalate records the repeat onto the escalation that already represents it.
const unnamedEscalationKeyPrefix = "seat-escalated-unnamed:"

// unnamedEscalationNotesTail is how many characters of the work bead's notes the escalation
// carries: the seat's last words, where gt done's refusals and the seat's own account sit.
const unnamedEscalationNotesTail = 300

// FileUnnamedEscalation implements patrolscan.UnnamedEscalationFiler (gt-7046im): it raises the
// escalation a seat should have filed when it stopped on ESCALATED, naming the seat, the bead and
// what to do. A delivery error is returned wrapped with the seat and the bead; the tick reports
// it in the seat's detail and tries again on the next tick.
func (h *patrolScanHost) FileUnnamedEscalation(rig, name, bead string) error {
	message := fmt.Sprintf("%s/%s exited ESCALATED on %s but filed no escalation naming it: read its last notes (bd show %s, gt peek %s/%s), then answer, or nuke the seat and re-sling the bead. %s",
		rig, name, bead, bead, rig, name, h.unnamedNotes(rig, bead))
	if err := h.d.escalateAlertErr(unnamedEscalationKeyPrefix+bead, "patrol_scan", message); err != nil {
		return fmt.Errorf("filing the unnamed escalation of %s/%s for %s: %w", rig, name, bead, err)
	}
	return nil
}

// unnamedNotes is the notes line of the escalation: the last unnamedEscalationNotesTail
// characters of the bead's notes, or the failure of the read, which never stops the filing.
func (h *patrolScanHost) unnamedNotes(rig, bead string) string {
	issue, err := h.d.workBeads(h.d.workBeadsEnv(rig), workBeadReadTimeout).Show(bead)
	if err != nil {
		return "notes unreadable: " + err.Error()
	}
	notes := []rune(strings.TrimSpace(issue.Notes))
	if len(notes) == 0 {
		return "its notes are empty"
	}
	if len(notes) > unnamedEscalationNotesTail {
		notes = notes[len(notes)-unnamedEscalationNotesTail:]
	}
	return "its last notes: " + string(notes)
}
