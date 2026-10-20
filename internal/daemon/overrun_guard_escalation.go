package daemon

import (
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// The escalation exemption: the time a seat spends waiting on an open escalation
// it raised does not count toward the 12 and 20 minute rule (gt-i1qwy). A polecat
// that finishes its work and escalates a question is waiting on a person, not
// overrunning. Charging that wait stopped a finished seat as needs-split.
//
// The tie is patrolscan.SeatEscalationVerdict, the definition the stuck-seat tick
// already reads: an escalation the seat raised (escalated_by is its own address)
// that names the seat's bead, by related_bead or by the title prefix. This guard
// only reads it, and never writes an escalation. The banking of the time is in
// overrun_guard_decide.go (withEscalation).

// overrunEscalationReader answers "is this seat waiting on an open escalation it
// raised" for one cycle. A rig's escalations are read once per cycle and shared
// by its seats. A failed read is kept with them, so every seat of the rig gets
// the same answer.
type overrunEscalationReader struct {
	d      *Daemon
	raised map[string]overrunRaised
}

// overrunRaised is one rig's escalations as this cycle read them, or the error
// the read failed with.
type overrunRaised struct {
	escalations []patrolscan.Escalation
	err         error
}

// newOverrunEscalationReader returns the reader for one cycle.
func newOverrunEscalationReader(d *Daemon) *overrunEscalationReader {
	return &overrunEscalationReader{d: d, raised: map[string]overrunRaised{}}
}

// waiting reports whether the seat has an open escalation it raised for its bead.
// An error means the escalations could not be read. The caller then counts all of
// the seat's time, the direction that stops rather than lets a seat run unbounded.
func (r *overrunEscalationReader) waiting(seat overrunSeat) (bool, error) {
	raised, ok := r.raised[seat.Rig]
	if !ok {
		escalations, err := (&patrolScanHost{d: r.d}).PolecatEscalations(seat.Rig)
		raised = overrunRaised{escalations: escalations, err: err}
		r.raised[seat.Rig] = raised
	}
	if raised.err != nil {
		return false, raised.err
	}
	verdict := patrolscan.SeatEscalationVerdict(seat.Rig, seat.Polecat, seat.Bead, raised.escalations)
	return verdict.Verdict == patrolscan.EscalationPending, nil
}
