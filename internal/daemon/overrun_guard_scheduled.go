package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/polecat"
)

// overrunScheduledPauseReason is the pause marker's reason for a scheduled
// stop, the record gt session stop also writes, so patrol_scan and the zombie
// detector leave the seat down.
const overrunScheduledPauseReason = "overrun (scheduled run limit)"

// overrunScheduledStop is the stop for a seat on a scheduled sling that ran
// past scheduled_stop_after. It shares the first steps of overrunStop and
// ends early, because a scheduled run is report-only and the scheduled_slings
// patrol creates the next one:
//
//  1. pause the seat, so patrol_scan does not restart it;
//  2. stop its session (a failed stop clears the pause again and is retried
//     on the next tick, from the seat list);
//  3. add one comment to the bead naming the limit.
//
// It does not hand the bead back, label it needs-split, checkpoint the
// worktree, push a branch or release the seat: the bead's status, assignee
// and labels stay as they were, and the seat stays paused as after gt session
// stop. A step that fails after the session is gone is retried from the saved
// state (resumeOverrunStops), and a comment that keeps failing is given up
// after overrunCommentMaxAttempts.
func (d *Daemon) overrunScheduledStop(s overrunSeams, seat overrunSeat, st overrunState, elapsed time.Duration) {
	who := seat.Rig + "/" + seat.Polecat
	now := s.now()
	save := func() bool {
		if err := writeOverrunState(d.config.TownRoot, st); err != nil {
			d.logger.Printf("overrun_guard: WARNING: %s: cannot save stop progress for %s: %v", who, st.Bead, err)
			return false
		}
		return true
	}

	// A stop that already finished belongs to an earlier sling (the caller
	// checked): start this one clean, keeping what the nudge remembers.
	if !st.StoppedAt.IsZero() {
		st = overrunState{Bead: st.Bead, NudgedAt: st.NudgedAt}
	}
	if st.StopBeganAt.IsZero() {
		st.Rig, st.Polecat, st.Branch, st.Elapsed, st.Scheduled = seat.Rig, seat.Polecat, seat.Branch, elapsed, true
	}
	// StopBeganAt decides, not SessionStoppedAt: a failed clearPause or save
	// leaves the stop begun with the session not yet recorded as stopped.
	resumed := !st.StopBeganAt.IsZero()
	elapsed = st.Elapsed
	seat.Scheduled = true

	// 1. Pause, after looking again: a seat that has begun gt done since the
	// seat list was read is not stopped.
	if st.StopBeganAt.IsZero() {
		if s.submittedNow != nil && s.submittedNow(seat) {
			d.logger.Printf("overrun_guard: %s on %s submitted before the stop; leaving it", who, st.Bead)
			return
		}
		if err := s.pause(seat); err != nil {
			d.logger.Printf("overrun_guard: stop scheduled %s on %s: pause failed: %v", who, st.Bead, err)
			return
		}
		st.StopBeganAt = now
		if !save() {
			// A pause nobody remembers is a parked seat nobody will stop.
			if err := s.clearPause(seat); err != nil {
				d.logger.Printf("overrun_guard: WARNING: %s: pause marker not cleared: %v", who, err)
			}
			return
		}
	}

	// 2. Stop the session. A resumed stop looks first: an operator may have
	// lifted the pause and started a new session, which stopSession would kill.
	if st.SessionStoppedAt.IsZero() {
		if resumed && s.stopLapsed != nil {
			why, err := s.stopLapsed(seat, false)
			if err != nil {
				d.logger.Printf("overrun_guard: stop scheduled %s on %s: cannot re-check the seat: %v", who, st.Bead, err)
				return
			}
			if why != "" {
				d.logger.Printf("overrun_guard: scheduled %s on %s: %s; abandoning the stop, session left running", who, st.Bead, why)
				// A save that fails is only warned of: the next tick reads the
				// same seat and abandons again, and stops nothing meanwhile.
				st.StoppedAt = now
				save()
				return
			}
		}
		if err := s.stopSession(seat); err != nil {
			d.logger.Printf("overrun_guard: stop scheduled %s on %s: session stop failed: %v", who, st.Bead, err)
			if cerr := s.clearPause(seat); cerr != nil {
				// Keep the stop pending: the next tick tries the stop again.
				d.logger.Printf("overrun_guard: WARNING: %s: pause marker not cleared: %v", who, cerr)
				return
			}
			st.StopBeganAt = time.Time{}
			if !save() {
				// The saved stop stays begun, and a begun stop with no pause
				// marker reads as an operator resume: put the pause back so the
				// two agree and the next tick stops the session again.
				if perr := s.pause(seat); perr != nil {
					d.logger.Printf("overrun_guard: WARNING: %s: pause marker not restored: %v", who, perr)
				}
			}
			return
		}
		d.logger.Printf("overrun_guard: stopped scheduled %s on %s after %s", who, st.Bead, elapsed.Round(time.Second))
		st.SessionStoppedAt = now
		if !save() {
			return
		}
	}

	// A stop resumed after its session was stopped may find the seat taken
	// back: an operator lifted the pause, or a new session started on it. The
	// comment would then describe a run that is not stopped, so the stop ends.
	if resumed && s.stopLapsed != nil {
		why, err := s.stopLapsed(seat, true)
		if err != nil {
			d.logger.Printf("overrun_guard: stop scheduled %s on %s: cannot re-check the seat: %v", who, st.Bead, err)
			return
		}
		if why != "" {
			st.StoppedAt = now
			save()
			d.logger.Printf("overrun_guard: scheduled %s on %s: %s; abandoning the stop, no comment added", who, st.Bead, why)
			return
		}
	}

	limit := overrunScheduledStopAfter(d.patrolConfig)
	target := seat
	target.Bead = st.Bead

	// 3. Comment on the bead, once: a later step that fails is retried from
	// the saved state and must not repeat the comment.
	if st.CommentedAt.IsZero() {
		if s.comment != nil {
			text := fmt.Sprintf("scheduled run stopped by the overrun guard after %s: it ran past "+
				"daemon.patrols.overrun_guard.scheduled_stop_after (%s). The run is report-only: "+
				"the guard closes this bead and releases the seat, and the scheduled_slings patrol "+
				"creates the next run.", elapsed.Round(time.Second), limit)
			if err := s.comment(target, text); err != nil {
				st.CommentFailures++
				d.logger.Printf("overrun_guard: stop scheduled %s on %s: comment failed (%d of %d): %v",
					who, st.Bead, st.CommentFailures, overrunCommentMaxAttempts, err)
				if st.CommentFailures < overrunCommentMaxAttempts {
					save()
					return
				}
				d.logger.Printf("overrun_guard: stop scheduled %s on %s: giving up on the comment; the stop goes on", who, st.Bead)
			}
		}
		st.CommentedAt = now
		if !save() {
			return
		}
	}

	// 4. Close the run bead. A scheduled run is report-only and its schedule
	// makes the next one, so there is no work to hand back: left hooked, the
	// bead would hold the seat against the town total and keep the entry in
	// skip-open until someone closed it by hand. The assignee is cleared first,
	// since bd refuses a close by an actor that is not the assignee.
	if st.ClosedAt.IsZero() && s.closeBead != nil {
		reason := fmt.Sprintf("stopped by the overrun guard after %s (scheduled_stop_after %s); "+
			"a scheduled run is report-only and the scheduled_slings patrol creates the next one",
			elapsed.Round(time.Second), limit)
		if err := s.closeBead(target, reason); err != nil {
			st.CloseFailures++
			d.logger.Printf("overrun_guard: stop scheduled %s on %s: close failed (%d of %d): %v",
				who, st.Bead, st.CloseFailures, overrunCommentMaxAttempts, err)
			if st.CloseFailures < overrunCommentMaxAttempts {
				save()
				return
			}
			// The bead is still hooked, so the seat is not released: it stays
			// paused for an operator, as a scheduled stop left it before.
			st.StoppedAt = now
			save()
			d.logger.Printf("overrun_guard: stop scheduled %s on %s: giving up on closing the bead; the seat stays paused (close it and nuke the seat by hand)", who, st.Bead)
			return
		}
		st.ClosedAt = now
		if !save() {
			return
		}
	}

	// 5. Release the seat, in the order overrunStop releases one: reset the
	// agent bead (it still names the closed run), record the cleanup status
	// the reset clears (so gt polecat nuke accepts the seat), and retire last
	// (ends the pause and stops the seat's record in one write). Every step
	// runs while the pause still keeps a new sling off the name; a failure
	// leaves the stop pending and the next tick repeats from here.
	if st.ClosedAt.IsZero() {
		// No close seam: the bead is still hooked, so leave the seat paused.
		st.StoppedAt = now
		save()
		return
	}
	if s.resetAgentBead != nil {
		if err := s.resetAgentBead(target); err != nil {
			d.logger.Printf("overrun_guard: WARNING: %s: agent bead not reset after closing %s: %v", who, st.Bead, err)
			return
		}
	}
	if s.recordCleanup != nil {
		if err := s.recordCleanup(seat); err != nil {
			d.logger.Printf("overrun_guard: WARNING: %s: cleanup status not recorded for the released seat %s: %v", who, st.Bead, err)
		}
	}
	if s.retire != nil {
		if err := s.retire(seat); err != nil {
			d.logger.Printf("overrun_guard: WARNING: %s: seat not retired after closing %s: %v", who, st.Bead, err)
			return
		}
	}
	st.StoppedAt = now
	if !save() {
		d.logger.Printf("overrun_guard: WARNING: %s: %s released but state not saved — the release may be repeated", who, st.Bead)
		return
	}
	d.logger.Printf("overrun_guard: released scheduled %s: run bead %s closed, seat retired", who, st.Bead)
}

// overrunCommentBead is the production comment seam: one comment on the seat's
// bead, written as the guard.
func (d *Daemon) overrunCommentBead(seat overrunSeat, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), overrunHandBackTimeout)
	defer cancel()
	store := beads.NewPlain(d.config.TownRoot, bdMutationRoutingEnv(d.config.TownRoot),
		beads.WithBin(d.bdPathOrDefault())).WithTimeout(overrunHandBackTimeout)
	return polecat.NewBeadWriter(store, overrunActor).Comment(ctx, seat.Bead, text)
}

// overrunCloseRunBead is the production closeBead seam: clear the assignee, then
// close the run bead with reason. The close is forced past dependency checks, as
// gt done's is: the run's molecule wisps must not hold a stopped run open. A bead
// that is no longer this seat's is left alone and counts as closed for the stop.
func (d *Daemon) overrunCloseRunBead(seat overrunSeat, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), overrunHandBackTimeout)
	defer cancel()
	store := beads.NewPlain(d.config.TownRoot, bdMutationRoutingEnv(d.config.TownRoot),
		beads.WithBin(d.bdPathOrDefault())).WithTimeout(overrunHandBackTimeout)
	skipped, err := closeRunBead(ctx, polecat.NewBeadWriter(store, overrunActor), store.ForceCloseWithReason,
		seat.Bead, seat.Rig+"/polecats/"+seat.Polecat, reason)
	if skipped != "" {
		d.logger.Printf("overrun_guard: %s/%s: %s; not closing it", seat.Rig, seat.Polecat, skipped)
	}
	if err == nil && skipped == "" {
		// The close ends the bead's run on the clock the guard and the
		// dashboard read, as gt bead close does, so a run bead dispatched again
		// after it starts over (gt-tzbtf). Best-effort: the stop's own steps
		// must not be held by the log.
		if lerr := events.LogAuditTo(d.config.TownRoot, events.TypeBeadClosed, overrunActor,
			map[string]interface{}{"bead": seat.Bead}); lerr != nil {
			d.logger.Printf("overrun_guard: WARNING: %s/%s: close of %s not recorded on the town log: %v",
				seat.Rig, seat.Polecat, seat.Bead, lerr)
		}
	}
	return err
}

// closeRunBead clears the assignee and force-closes the run bead, after
// reading who holds it. A close that failed after the assignee was cleared is
// retried from the next tick, and in between the dispatcher may have slung the
// unassigned bead to another polecat or it may have closed: either way the
// bead is no longer assignee's, so nothing is written and skipped says why. An
// empty assignee is the half-closed bead and is finished.
func closeRunBead(ctx context.Context, w polecat.BeadWriter, force func(reason string, ids ...string) error,
	beadID, assignee, reason string) (skipped string, err error) {
	status, current, err := w.State(ctx, beadID)
	if err != nil {
		return "", fmt.Errorf("close %s: read bead: %w", beadID, err)
	}
	if status == "closed" || status == "tombstone" {
		return fmt.Sprintf("%s is already %s", beadID, status), nil
	}
	if current != "" && current != assignee {
		return fmt.Sprintf("%s is assigned to %s, not %s", beadID, current, assignee), nil
	}
	if err := w.ClearAssignee(ctx, beadID); err != nil {
		return "", err
	}
	return "", force(reason, beadID)
}
