package daemon

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
)

const (
	// seatUnreachableReleaseAfter is how long a hooked seat's session may stay
	// unreadable before the daemon takes the stop path for it: at
	// seatUnreachableAfter the pass names the seat, and at this one the seat
	// has plainly gone and the bead it holds must go back to the pool
	// (gt-wgim24).
	seatUnreachableReleaseAfter = 15 * time.Minute
	// seatUnreachablePauseReason is the reason on the pause marker the release
	// writes, and seatUnreachableActor signs it. The release reads the pair
	// back to finish a park an earlier tick of this path left behind: a failed
	// release must not leave the seat parked with nothing to release it.
	seatUnreachablePauseReason = "seat unreachable (15 minute rule)"
	seatUnreachableActor       = "seat_unreachable"
)

// seatUnreachableStopSeams are the stop path's side effects for a seat the
// polecat-health pass cannot read. Production fills them from the guard's own
// stop seams (overrunStopSeams), so the pause, the sandbox removal, the
// hand-back, the agent-bead reset, the cleanup status and the retire are the
// overrun stop's, and a seat released because it is unreachable reads as
// released as one the stop released. Only the pause (whose reason and actor
// have to say why this path stopped the seat) and the branch the hand-back
// comment names are this path's own; the rest are the guard's.
//
// pause and handBack are required: a release that cannot park the seat or hand
// the bead back is no release, so a nil one refuses it rather than acting on
// half of it. The rest follow the overrun stop's "nil means none" — a host seat
// with no sandbox record, a test with no retire — and each nil one leaves that
// single step out.
type seatUnreachableStopSeams struct {
	// pause parks the seat while it is released, so no new sling lands on the
	// name before the hand-back and the retire are done. Required.
	pause func(seat overrunSeat) error
	// removeSandbox deletes the gateway sandbox object the seat ran in, through
	// the overrun stop's own removal: a released seat whose sandbox is left in
	// phase Error holds the name, and the next sling to it fails. nil means no
	// sandbox step (a host seat, or a test that does not exercise it).
	removeSandbox func(seat overrunSeat) error
	// beadStillHeld re-reads the bead just before the hand-back and reports
	// whether it is still hooked to this seat. Required: a release that cannot
	// tell whose bead it would hand back must not hand it back. A bead that has
	// already gone back to the pool, or moved to another seat, is not this
	// seat's any more, and handing it back again would write the note and the
	// label onto someone else's run.
	beadStillHeld func(seat overrunSeat) (bool, error)
	// handBack opens the bead with the note, hands it back to the pool and
	// ends the molecule it carried. Required.
	handBack func(seat overrunSeat, elapsed time.Duration, note string) error
	// resetAgentBead clears the seat's agent bead of the handed-back bead;
	// recordCleanup records the cleanup status that reset leaves missing; and
	// retire ends the seat's hold and stops its record in one write.
	resetAgentBead func(seat overrunSeat) error
	recordCleanup  func(seat overrunSeat) error
	retire         func(seat overrunSeat) error
	// submittedNow re-reads whether the seat has submitted its work since the
	// seat list was read. nil means no re-check.
	submittedNow func(seat overrunSeat) bool
	// slungAt dates the bead's dispatch, zero and false when it is unknown. It
	// keeps the hand-back comment's length the run's own rather than the age of
	// the outage, which only says when the reads began failing.
	slungAt func(seat overrunSeat) (time.Time, bool)
	// branch names the worktree branch the hand-back comment carries.
	branch func(seat overrunSeat) string
}

// stopSeamsForUnreachableSeats fills the release's seams from the guard's own
// stop seams, so nothing is copied from the stop: the release is the stop's
// steps 1, 2's sandbox removal, 5 and 6 over a seat whose session answers
// nothing.
func (d *Daemon) stopSeamsForUnreachableSeats(s overrunSeams) seatUnreachableStopSeams {
	return seatUnreachableStopSeams{
		pause: func(seat overrunSeat) error {
			return agentpause.Pause(d.config.TownRoot, seat.Rig, constants.RolePolecat, seat.Polecat,
				seatUnreachablePauseReason, seatUnreachableActor, "")
		},
		removeSandbox:  s.removeSandbox,
		beadStillHeld:  s.beadStillHeld,
		handBack:       s.handBack,
		resetAgentBead: s.resetAgentBead,
		recordCleanup:  s.recordCleanup,
		retire:         s.retire,
		submittedNow:   s.submittedNow,
		slungAt:        s.slungAt,
		branch:         d.seatUnreachableBranch,
	}
}

// seatUnreachableBranch names the branch the seat's worktree is on, for the
// hand-back comment to carry. A seat with no worktree has none, and a branch
// that cannot be read is logged and left unnamed: the release goes on, since
// the bead coming back to the pool matters more than its comment.
func (d *Daemon) seatUnreachableBranch(seat overrunSeat) string {
	polecatsDir := filepath.Join(d.config.TownRoot, seat.Rig, "polecats")
	workDir := resolvePolecatWorktree(polecatsDir, seat.Polecat, seat.Rig)
	if workDir == "" {
		return ""
	}
	branch, err := d.gitAt(workDir).CurrentBranch()
	if err != nil {
		d.logger.Printf("seat_unreachable: %s/%s: cannot read the branch of its worktree %s: %v",
			seat.Rig, seat.Polecat, workDir, err)
		return ""
	}
	return branch
}

// seatUnreachableBeadStillHeld is the release's own bead read: a bead is the
// seat's to hand back only while it is still hooked to it (gt-wgim24).
func (d *Daemon) seatUnreachableBeadStillHeld(seat overrunSeat) (bool, error) {
	status, assignee, err := d.overrunBeadState(seat)
	if err != nil {
		return false, fmt.Errorf("reading bead %s: %w", seat.Bead, err)
	}
	return seatUnreachableHoldsBead(seat, status, assignee), nil
}

// seatUnreachableHoldsBead reports whether a bead in this status and assignee is
// still hooked to the seat. The assignee must be the seat's, and the bead must
// not be open — the release already handed it back, or someone reopened it — or
// closed. The hand-back's own read treats an open, unassigned bead as a
// half-finished hand-back; this path is past that point, so it ends instead.
func seatUnreachableHoldsBead(seat overrunSeat, status, assignee string) bool {
	if assignee != overrunSeatAssignee(seat) {
		return false
	}
	switch status {
	case "open", "closed", "tombstone":
		return false
	}
	return true
}

// outagesAt returns the tracker's first-failure times, one per seat, in seat
// order. It is a copy taken under the watch's lock, so the guard can act on
// what the polecat-health pass observed without holding up that pass.
func (w *seatUnreachableWatch) outagesAt() []seatUnreachable {
	w.mu.Lock()
	out := make([]seatUnreachable, 0, len(w.outages))
	for key, outage := range w.outages {
		out = append(out, seatUnreachable{Rig: key.rig, Polecat: key.polecat, Bead: outage.bead,
			Since: outage.since, Err: outage.err, Released: outage.released})
	}
	w.mu.Unlock()
	slices.SortFunc(out, func(a, b seatUnreachable) int {
		return cmp.Or(cmp.Compare(a.Rig, b.Rig), cmp.Compare(a.Polecat, b.Polecat))
	})
	return out
}

// markReleased records on the seat's outage that the daemon released it, keyed by
// the seat and the outage's first-failure time: a marker written for one outage
// does not silence the next, and a tick whose pass has already moved the seat on
// (a good read cleared it, or a new outage started) leaves it unmarked
// (gt-wgim24).
func (w *seatUnreachableWatch) markReleased(key seatKey, since time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	outage, ok := w.outages[key]
	if !ok || !outage.since.Equal(since) {
		return
	}
	outage.released = true
	w.outages[key] = outage
}

// seatUnreachableDue names the seats among outages whose session has answered
// nothing for seatUnreachableReleaseAfter at now. A seat that has been readable
// since, or that never got a failed read, is not in outages at all, and one
// already released for this outage is marked, so both are left alone by
// construction.
func seatUnreachableDue(outages []seatUnreachable, now time.Time) []seatUnreachable {
	var due []seatUnreachable
	for _, out := range outages {
		if out.Released || now.Sub(out.Since) < seatUnreachableReleaseAfter {
			continue
		}
		due = append(due, out)
	}
	return due
}

// seatUnreachableNote is the hand-back note for a seat the pass could not read:
// it names the seat, how long it has been unreadable and the error its last read
// returned, so whoever finds the bead back in the pool reads why it came back.
// The error's text is a session read's, which can carry the output of the
// command that failed, so the note gets one bounded line of it (oneLineReason),
// never the whole capture.
func seatUnreachableNote(alert seatUnreachable, now time.Time) string {
	return fmt.Sprintf("seat %s/%s unreachable for %s: %s",
		alert.Rig, alert.Polecat, parkedAge(now.Sub(alert.Since)), oneLineReason(alert.Err))
}

// releaseUnreachableSeats takes the stop path for every seat the polecat-health
// pass has found unreadable for seatUnreachableReleaseAfter at now, and for no
// other seat. No nudge is sent: a nudge is delivered to a session, and this one
// answers nothing.
//
// The seat list the guard judges cannot hold these seats — overrunWorkingSeats
// reads each seat's session first and drops a seat it cannot read — so this is
// where the daemon stops and releases them. A release that fails is logged and
// left to the next tick: the tracker keeps the outage while the seat stays
// unreadable, and every step of the release is safe to repeat. A release that
// ends marks its outage, so the seat is not released again until a good read
// clears the outage. In a dry run nothing is paused, written, marked or
// released, and the log says what would have been.
func (d *Daemon) releaseUnreachableSeats(s seatUnreachableStopSeams, now time.Time, dry bool) {
	for _, alert := range seatUnreachableDue(d.seatUnreachable.outagesAt(), now) {
		who := alert.Rig + "/" + alert.Polecat
		if dry {
			d.logger.Printf("seat_unreachable: dry run: would release %s held by %s, unreadable for %s",
				alert.Bead, who, parkedAge(now.Sub(alert.Since)))
			continue
		}
		if err := d.releaseUnreachableSeat(s, alert, now); err != nil {
			// A step's error can carry the output of the command that failed
			// (a push, a bd write), so the log gets one bounded line of it,
			// never the whole capture.
			d.logger.Printf("seat_unreachable: %s: %s", who, oneLineReason(err))
		}
	}
}

// releaseUnreachableSeat releases one seat's bead and the seat with it: pause
// the seat, delete the gateway sandbox object it ran in, hand the bead back to
// the pool with seatUnreachableNote, reset the seat's agent bead, record the
// cleanup status that reset clears and retire the seat. A seat with nothing the
// daemon may release — no bead it could read, a record that is held or
// submitted — is left alone and logged once.
//
// The sandbox goes with the release because a sandbox left behind, in phase
// Error or merely stopped, keeps the seat's name bound: the dispatcher reuses
// the name and its next slings fail on the stale object (gt-wgim24, operator
// decision 2026-11-14). The removal is the overrun stop's own, so it asks the
// push first and keeps the object when the work cannot be shown to have reached
// origin.
//
// The hold is re-read here and not taken on the pass's word: the tracker carries
// an outage through the passes that skip a seat (gt-q6hafz), so a seat an
// operator parks after its outage began still has a running outage. The one park
// the release does not read as a hold is its own: a release that stopped between
// its pause and its retire leaves the seat parked by seatUnreachableActor, and
// refusing that park would leave the bead held for good.
//
// Two guards keep a released seat from being released again, since the seat
// stays unreadable and parked, and a release that ends marks the outage rather
// than trusting the next tick to find nothing left to do: the bead is re-read
// before the seat is touched, and a bead that is no longer hooked to it — one an
// earlier tick already returned to the pool, or the dispatcher slung elsewhere —
// ends the release with nothing done to the seat (gt-wgim24).
func (d *Daemon) releaseUnreachableSeat(s seatUnreachableStopSeams, alert seatUnreachable, now time.Time) error {
	who := alert.Rig + "/" + alert.Polecat
	skip := func(format string, args ...any) error {
		d.crashSkipLog.logf(d.logger.Printf, "seat-unreachable:"+who, format, args...)
		return nil
	}
	if s.pause == nil || s.beadStillHeld == nil || s.handBack == nil {
		// One half of a release is no release: the seat would end up parked
		// with its bead, or its bead handed back with the seat still holding
		// it, or a bead handed back with nothing having read whose it is. An
		// unwired seam refuses instead of acting on the other steps.
		return fmt.Errorf("release seams incomplete (pause %t, bead read %t, hand-back %t): refusing to release %s",
			s.pause != nil, s.beadStillHeld != nil, s.handBack != nil, alert.Bead)
	}
	if alert.Bead == "" {
		return skip("seat_unreachable: %s holds no bead the pass could read; nothing to release", who)
	}
	rec, err := intent.Read(d.config.TownRoot, intent.Seat{Rig: alert.Rig, Role: constants.RolePolecat, Name: alert.Polecat})
	if err != nil {
		// intent.Read fails closed: an unreadable record says the seat is held,
		// and an operator hold is never released on a guess.
		return fmt.Errorf("reading its intent record: %w", err)
	}
	ours := !rec.Frozen && rec.Desired == intent.DesiredPark && rec.PausedBy == seatUnreachableActor
	if rec.Held() && !ours {
		return skip("seat_unreachable: %s is held (%s); leaving its bead %s alone", who, rec.HoldReason(), alert.Bead)
	}
	if rec.Submitted() {
		return skip("seat_unreachable: %s submitted %s for landing; the landing worker holds it", who, alert.Bead)
	}
	seat := overrunSeat{Rig: alert.Rig, Polecat: alert.Polecat, Bead: alert.Bead}
	if s.branch != nil {
		seat.Branch = s.branch(seat)
	}
	if s.submittedNow != nil && s.submittedNow(seat) {
		return skip("seat_unreachable: %s submitted %s while unreadable; the landing worker holds it", who, alert.Bead)
	}
	elapsed := now.Sub(alert.Since)
	if s.slungAt != nil {
		if start, ok := s.slungAt(seat); ok && !start.After(now) {
			elapsed = now.Sub(start)
		}
	}
	// The bead is re-read before anything is written for the seat: an earlier
	// tick of this path may already have handed it back, or the dispatcher may
	// have slung it to another seat, and either way it is not this seat's to
	// hand back again. That ends the release as done, with nothing done to the
	// seat. The hand-back re-reads it too, so a bead that moves while the seat
	// is released is still refused there.
	held, err := s.beadStillHeld(seat)
	if err != nil {
		return fmt.Errorf("re-reading %s before releasing the seat: %w", alert.Bead, err)
	}
	if !held {
		d.seatUnreachable.markReleased(seatKey{rig: alert.Rig, polecat: alert.Polecat}, alert.Since)
		d.logger.Printf("seat_unreachable: %s: %s is no longer hooked to it (open, closed or moved); the release is done",
			who, alert.Bead)
		return nil
	}
	if err := s.pause(seat); err != nil {
		return fmt.Errorf("pausing the seat after %s unreadable: %w", parkedAge(now.Sub(alert.Since)), err)
	}
	// The sandbox goes before the hand-back, as in the overrun stop: a refused
	// removal keeps the object and stops the release here, with the seat parked
	// and its bead still the seat's, so the next tick retries it. A seat with no
	// sandbox record has no step at all.
	if s.removeSandbox != nil {
		if err := s.removeSandbox(seat); err != nil {
			return fmt.Errorf("sandbox not removed after %s unreadable: %w", parkedAge(now.Sub(alert.Since)), err)
		}
	}
	if err := s.handBack(seat, elapsed, seatUnreachableNote(alert, now)); err != nil {
		return fmt.Errorf("handing %s back: %w", alert.Bead, err)
	}
	if s.resetAgentBead != nil {
		if err := s.resetAgentBead(seat); err != nil {
			return fmt.Errorf("agent bead not reset after handing %s back: %w", alert.Bead, err)
		}
	}
	// The cleanup status holds no work back: a failed record is logged and the
	// release goes on, as the stop's does.
	if s.recordCleanup != nil {
		if err := s.recordCleanup(seat); err != nil {
			d.logger.Printf("seat_unreachable: WARNING: %s: cleanup status not recorded for the released seat %s: %v",
				who, alert.Bead, err)
		}
	}
	if s.retire != nil {
		if err := s.retire(seat); err != nil {
			return fmt.Errorf("seat not retired after handing %s back: %w", alert.Bead, err)
		}
	}
	// The seat is still unreadable and the pass skips a parked seat, so the
	// tracker would carry this outage and hand the same bead back every tick.
	// The mark ends the release for this outage; a good read drops the outage
	// and the mark with it (gt-wgim24).
	d.seatUnreachable.markReleased(seatKey{rig: alert.Rig, polecat: alert.Polecat}, alert.Since)
	d.logger.Printf("seat_unreachable: released %s held by %s, unreadable for %s: %s",
		alert.Bead, who, parkedAge(now.Sub(alert.Since)), oneLineReason(alert.Err))
	return nil
}
