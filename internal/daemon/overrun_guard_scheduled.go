package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
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
	resumed := !st.SessionStoppedAt.IsZero()
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

	// 2. Stop the session.
	if st.SessionStoppedAt.IsZero() {
		if err := s.stopSession(seat); err != nil {
			d.logger.Printf("overrun_guard: stop scheduled %s on %s: session stop failed: %v", who, st.Bead, err)
			if cerr := s.clearPause(seat); cerr != nil {
				// Keep the stop pending: the next tick tries the stop again.
				d.logger.Printf("overrun_guard: WARNING: %s: pause marker not cleared: %v", who, cerr)
				return
			}
			st.StopBeganAt = time.Time{}
			save()
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
		why, err := s.stopLapsed(seat)
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

	// 3. Comment on the bead.
	if s.comment != nil {
		text := fmt.Sprintf("scheduled run stopped by the overrun guard after %s: it ran past "+
			"daemon.patrols.overrun_guard.scheduled_stop_after (%s). The bead's status is unchanged; "+
			"the scheduled_slings patrol creates the next run.",
			elapsed.Round(time.Second), overrunScheduledStopAfter(d.patrolConfig))
		if err := s.comment(seat, text); err != nil {
			st.CommentFailures++
			d.logger.Printf("overrun_guard: stop scheduled %s on %s: comment failed (%d of %d): %v",
				who, st.Bead, st.CommentFailures, overrunCommentMaxAttempts, err)
			if st.CommentFailures < overrunCommentMaxAttempts {
				save()
				return
			}
			d.logger.Printf("overrun_guard: stop scheduled %s on %s: giving up on the comment; the session stays stopped", who, st.Bead)
		}
	}
	st.StoppedAt = now
	if !save() {
		d.logger.Printf("overrun_guard: WARNING: %s: %s stopped but state not saved — the comment may be repeated", who, st.Bead)
	}
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
