package daemon

import (
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// The escalation exemption: the time a seat spends waiting on an open escalation
// it raised does not count toward the 12 and 20 minute rule (gt-i1qwy). A polecat
// that finishes its work and escalates a question is waiting on a person, not
// overrunning. Charging that wait stopped a finished seat as needs-split.
//
// The tie is patrolscan.SeatEscalationHold, the definition the stuck-seat tick
// and the crash detector already read: the seat's record says it stopped on an
// escalation, and it raised one (escalated_by is its own address) that names the
// seat's bead, by related_bead or by the title prefix. This guard only reads it,
// and never writes an escalation. The banking of the time is in
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

// waiting reports whether the seat is stopped on an open escalation it raised for
// its bead: its record says it stopped on one and the escalation is still open
// (patrolscan.SeatEscalationHold). A live seat that raised an escalation and kept
// working is waiting on no one, so its time still counts.
//
// The rig's escalations are read first, once per cycle, and a seat with no open
// escalation of its own naming its bead is answered from them: it cannot be
// waiting whatever its record says, so its agent bead is not read. Only a seat
// with such an escalation pays for the record read.
// An error means the record or the escalations could not be read. The caller then
// counts all of the seat's time, the direction that stops rather than lets a seat
// run unbounded.
func (r *overrunEscalationReader) waiting(seat overrunSeat) (bool, error) {
	h := &patrolScanHost{d: r.d}
	raised, ok := r.raised[seat.Rig]
	if !ok {
		escalations, err := h.PolecatEscalations(seat.Rig)
		raised = overrunRaised{escalations: escalations, err: err}
		r.raised[seat.Rig] = raised
	}
	if raised.err != nil {
		return false, raised.err
	}
	if patrolscan.SeatEscalationVerdict(seat.Rig, seat.Polecat, seat.Bead, raised.escalations).Verdict != patrolscan.EscalationPending {
		return false, nil
	}
	rec, err := h.AgentRecord(seat.Rig, seat.Polecat)
	if err != nil {
		return false, err
	}
	verdict, err := patrolscan.SeatEscalationHold(rec, seat.Rig, seat.Polecat, seat.Bead, func() ([]patrolscan.Escalation, error) {
		return raised.escalations, nil
	})
	if err != nil {
		return false, err
	}
	return verdict.Verdict == patrolscan.EscalationPending, nil
}
