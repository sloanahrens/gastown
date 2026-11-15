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
	"github.com/steveyegge/gastown/internal/session"
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
// stop seams (overrunStopSeams), so the hand-back, the agent-bead reset, the
// cleanup status and the retire are the overrun stop's, and a seat released
// because it is unreachable reads as released as one the stop released. The
// pause (whose reason and actor have to say why this path stopped the seat), the
// branch the hand-back comment names, the origin read and the sandbox release are
// this path's own; the rest are the guard's.
//
// pause, the origin read, the sandbox release and handBack are required: a
// release that cannot park the seat, cannot say where the seat's work is, cannot
// free the seat's name or cannot hand the bead back is no release, so a nil one
// refuses it rather than acting on half of it. The rest follow the overrun
// stop's "nil means none" — a test with no retire — and each nil one leaves that
// single step out.
type seatUnreachableStopSeams struct {
	// pause parks the seat while it is released, so no new sling lands on the
	// name before the hand-back and the retire are done. Required.
	pause func(seat overrunSeat) error
	// originHolds asks origin whether it holds the branch the seat's work is on,
	// and names that branch. Required: a release that cannot tell where the work
	// is must not delete the seat's sandbox.
	originHolds func(seat overrunSeat) (branch string, held bool, err error)
	// releaseSandbox deletes the gateway sandbox the seat ran in and drops its
	// record in one step. Required: an object nobody deleted keeps the seat name
	// out of the dispatcher's draw until a person removes it.
	releaseSandbox func(seat overrunSeat) error
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
// stop seams, so nothing is copied from the stop: the release runs the stop's
// steps 1, 5 and 6 over a seat whose session answers nothing, and does the
// branch-and-sandbox step (2b) its own way, since the stop's asks the seat's
// sandbox and this seat's does not answer (gt-35ihg5).
func (d *Daemon) stopSeamsForUnreachableSeats(s overrunSeams) seatUnreachableStopSeams {
	return seatUnreachableStopSeams{
		pause: func(seat overrunSeat) error {
			return agentpause.Pause(d.config.TownRoot, seat.Rig, constants.RolePolecat, seat.Polecat,
				seatUnreachablePauseReason, seatUnreachableActor, "")
		},
		originHolds: d.seatUnreachableOriginHolds,
		releaseSandbox: func(seat overrunSeat) error {
			return d.seatUnreachableReleaseSandbox(seat, session.SandboxDeleter)
		},
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

// seatUnreachableOriginHolds reads origin for the branch the seat's work ran,
// from the town's own copy of the seat's worktree: the sandbox is often the
// thing that stopped answering, which is why the seat is being released, so it is
// never asked. The branch is the seat's own, or the sandbox record's when the
// seat names none, and branch is returned for the warning a branch origin does
// not hold carries.
//
// Origin is asked for every seat, whether or not it still has a sandbox record:
// a record an earlier tick already dropped is a retried release, and answering
// "held" without asking would read a branch origin does not hold as one it does
// (gt-35ihg5). A record that cannot be read is an error, as every read of it is,
// and so is a seat and a record that together name no branch, or a seat with no
// worktree to ask from: nothing can be asked about a branch nobody named, and
// the answer decides whether the sandbox may be deleted.
func (d *Daemon) seatUnreachableOriginHolds(seat overrunSeat) (string, bool, error) {
	sb, err := d.overrunSandboxSession(seat)
	if err != nil {
		return "", false, err
	}
	branch := seat.Branch
	if sb.Session != "" {
		rec, sandboxed := sb.Record, sb.Read
		if !sb.Read {
			rec, sandboxed, err = d.readSandboxRecordFor(d.config.TownRoot, sb.Session)
			if err != nil {
				return "", false, overrunRecordReadError(sb.Session, err)
			}
		}
		if sandboxed && (branch == "" || branch == "(unknown)") {
			branch = rec.Branch
		}
	}
	if branch == "" || branch == "(unknown)" {
		if sb.Session == "" {
			return "", false, fmt.Errorf("%s/%s names no branch to ask origin about", seat.Rig, seat.Polecat)
		}
		return "", false, fmt.Errorf("sandbox %s names no branch to ask origin about", sb.Session)
	}
	workDir := resolvePolecatWorktree(filepath.Join(d.config.TownRoot, seat.Rig, "polecats"), seat.Polecat, seat.Rig)
	if workDir == "" {
		return branch, false, fmt.Errorf("%s/%s has no worktree to read origin from", seat.Rig, seat.Polecat)
	}
	_, held, err := originBranchRead(d.gitAt(workDir), branch)
	if err != nil {
		return branch, false, err
	}
	return branch, held, nil
}

// seatUnreachableReleaseSandbox deletes the gateway sandbox the seat ran in and
// drops its record, through the record deleter: the object is deleted first (a
// sandbox the gateway no longer has counts as deleted, and the deleter copies
// the seat's transcripts before it goes), and the record — with the seat's
// end line for the ledger — goes only after the object, so no sandbox is left
// behind with nothing naming it and no record outlives its sandbox (gt-35ihg5).
// The release calls this only when origin holds the seat's branch, so the
// commits the sandbox's clone may still hold are on origin; commits beyond
// origin's tip are the one loss it cannot rule out, and the transcripts are
// copied out before the delete (gt-jhoi1).
//
// A deleter that is not linked refuses the step rather than dropping the record
// alone: a nil one leaves the sandbox and takes away the only handle on it. A
// record that cannot be read, a refused delete and a record that cannot be
// dropped are the step's errors and keep the seat owed, so the next tick retries
// it; the delete and the drop are both safe to repeat. A seat with no sandbox
// record has nothing to release.
func (d *Daemon) seatUnreachableReleaseSandbox(seat overrunSeat, deleter func(townRoot string, rec session.SandboxRecord) error) error {
	sb, err := d.overrunSandboxSession(seat)
	if err != nil {
		return err
	}
	if sb.Session == "" {
		return nil
	}
	if !sb.Read {
		rec, sandboxed, err := d.readSandboxRecordFor(d.config.TownRoot, sb.Session)
		if err != nil {
			return overrunRecordReadError(sb.Session, err)
		}
		if !sandboxed {
			return nil
		}
		sb.Record = rec
	}
	if deleter == nil {
		return fmt.Errorf("sandbox %s: no sandbox deleter is linked, so its record is kept with its sandbox", sb.Session)
	}
	if err := session.CleanupSandboxWithRecord(deleter, d.config.TownRoot, sb.Session, sb.Record); err != nil {
		return fmt.Errorf("sandbox %s: %w", sb.Session, err)
	}
	d.logger.Printf("seat_unreachable: %s/%s: deleted sandbox %s and dropped its record", seat.Rig, seat.Polecat, sb.Session)
	return nil
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

// raiseSeatUnreachableBranchAbsent warns about, and raises the item for, a seat
// whose branch origin answered it does not hold. The bead goes back to the pool
// anyway, because a bead back in the pool is worth more than a seat nobody can
// read — but the seat's commits may lie only in a sandbox that is still there,
// so the release keeps that sandbox and its record for a person and this warning
// names the seat, the branch and what origin answered. A raise that fails is
// logged and holds nothing back, as the overrun stop's own sandbox-gone raise
// does: the next tick raises the same item under the same key (gt-mp9zrg).
func (d *Daemon) raiseSeatUnreachableBranchAbsent(alert seatUnreachable, branch string) {
	who := alert.Rig + "/" + alert.Polecat
	d.logger.Printf("seat_unreachable: WARNING: %s: origin holds no branch %s, so the seat's commits may be nowhere; keeping its sandbox and record, and handing %s back once",
		who, branch, alert.Bead)
	message := fmt.Sprintf("the seat-unreachable release of %s on %s found origin holds no branch %s: the seat's commits may have gone with its sandbox, and no copy is on origin. Its sandbox and record are kept, so anything still on it can be recovered.",
		who, alert.Bead, branch)
	if err := d.escalateAlertSeverity("MEDIUM", "seat-unreachable-sandbox-gone:"+who+":"+alert.Bead, seatUnreachableActor, message); err != nil {
		d.logger.Printf("seat_unreachable: WARNING: %s: the sandbox-gone item for %s was not raised: %s",
			who, alert.Bead, oneLineReason(err))
	}
}

// raiseSeatUnreachableReleaseFailure raises the item a failed release leaves
// behind. The seat is still owed its release — nothing marked the outage
// released, so the next tick runs every step again — and the item says so,
// naming the seat, the bead, how long it has been unreadable and one bounded
// line of the step's error, so a seat the release cannot finish reaches a
// person rather than sitting out the outage unnoticed (gt-35ihg5). A raise that
// fails is logged and changes nothing: the next tick raises it again and the
// seat stays owed whatever happens here.
func (d *Daemon) raiseSeatUnreachableReleaseFailure(alert seatUnreachable, now time.Time, err error) {
	who := alert.Rig + "/" + alert.Polecat
	reason := oneLineReason(err)
	d.logger.Printf("seat_unreachable: WARNING: %s: the release of %s failed and stays owed to the next tick: %s",
		who, alert.Bead, reason)
	message := fmt.Sprintf("the seat-unreachable release of %s on %s failed after %s unreadable; its bead stays held and every tick retries it: %s",
		who, alert.Bead, parkedAge(now.Sub(alert.Since)), reason)
	if raiseErr := d.escalateAlertSeverity("MEDIUM", "seat-unreachable-release:"+who+":"+alert.Bead, seatUnreachableActor, message); raiseErr != nil {
		d.logger.Printf("seat_unreachable: WARNING: %s: the release-failure item for %s was not raised: %s",
			who, alert.Bead, oneLineReason(raiseErr))
	}
}

// releaseUnreachableSeats takes the stop path for every seat the polecat-health
// pass has found unreadable for seatUnreachableReleaseAfter at now, and for no
// other seat. No nudge is sent: a nudge is delivered to a session, and this one
// answers nothing.
//
// The seat list the guard judges cannot hold these seats — overrunWorkingSeats
// reads each seat's session first and drops a seat it cannot read — so this is
// where the daemon stops and releases them. A release that fails stays owed to
// the next tick: nothing marks the outage released, the tracker keeps it while
// the seat stays unreadable, and every step of the release is safe to repeat.
// The failure is raised as an item as well as logged, so a seat no step can
// finish reaches a person instead of sitting out the outage on a board that
// still shows it working (gt-35ihg5). A release that ends marks its outage, so
// the seat is not released again until a good read clears the outage. In a dry
// run nothing is paused, written, marked or released, and the log says what
// would have been.
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
			d.raiseSeatUnreachableReleaseFailure(alert, now, err)
		}
	}
}

// releaseUnreachableSeat releases one seat's bead and the seat with it: pause
// the seat, ask origin for the branch its work ran, release the seat's sandbox
// and record when origin holds that branch, hand the bead back to the pool with
// seatUnreachableNote, reset the seat's agent bead, record the cleanup status
// that reset clears and retire the seat. A seat with nothing the daemon may
// release — no bead it could read, a record that is held or submitted — is left
// alone and logged once.
//
// The seat's sandbox is never asked and never has to answer: a seat unreachable
// for fifteen minutes usually has a sandbox the gateway cannot speak for any
// more (its pod gone, its phase Error), and a step that waits on it strands the
// bead and the name together. Origin is the one place the work can still be
// vouched for, so the release asks it, leaves a branch it holds untouched, and
// deletes the sandbox and its record only on that answer — an object nobody
// deleted keeps the name out of the dispatcher's draw, so leaving it to the
// patrol does not free the name. Without that answer the sandbox and its record
// stay: they may hold the only copy of the work, and the item raised instead
// asks a person to look (gt-35ihg5).
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
	if s.pause == nil || s.originHolds == nil || s.releaseSandbox == nil ||
		s.beadStillHeld == nil || s.handBack == nil {
		// One half of a release is no release: the seat would end up parked
		// with its bead, or its bead handed back with the seat still holding
		// it, or a bead handed back with nothing having read whose it is, or its
		// sandbox deleted with nothing having asked where the work is. An
		// unwired seam refuses instead of acting on the other steps.
		return fmt.Errorf("release seams incomplete (pause %t, origin read %t, sandbox release %t, bead read %t, hand-back %t): refusing to release %s",
			s.pause != nil, s.originHolds != nil, s.releaseSandbox != nil,
			s.beadStillHeld != nil, s.handBack != nil, alert.Bead)
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
	// 2b. The branch and the sandbox, before the hand-back.
	//
	// A branch origin holds is left untouched: origin already has the work, so
	// the release writes nothing to it, and the sandbox may go with its record,
	// so the seat's name is free again. A branch origin does not hold is the
	// overrun stop's sandbox-gone case: the seat's commits may be nowhere, so
	// the release warns and raises before the bead goes back (gt-mp9zrg) and
	// keeps the sandbox and its record, which may be the only copy of the work.
	// A read that answered nothing is unknown, so the release waits for the next
	// tick: a sandbox deleted on a guess can take the only copy of the work.
	branch, onOrigin, err := s.originHolds(seat)
	if err != nil {
		return fmt.Errorf("origin not read for the seat's branch, %s unreadable: %w", parkedAge(now.Sub(alert.Since)), err)
	}
	if !onOrigin {
		d.raiseSeatUnreachableBranchAbsent(alert, branch)
	} else if err := s.releaseSandbox(seat); err != nil {
		// A sandbox that cannot be released stops the release here, with the
		// bead still the seat's, so the next tick retries it.
		return fmt.Errorf("sandbox of %s not released: %w", who, err)
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
