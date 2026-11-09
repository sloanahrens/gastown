package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/polecat/spans"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
)

const (
	// overrunActor signs the pause marker and the hand-back comment.
	overrunActor = "overrun_guard"
	// overrunPauseReason is the pause marker's reason.
	overrunPauseReason = "overrun (20 minute rule)"
	// overrunHandBackTimeout bounds the bd writes of one hand-back.
	overrunHandBackTimeout = 2 * time.Minute
)

// overrunStop is the 20-minute stop for one seat, in a fixed order, each step
// running only after the one before it has succeeded:
//
//  1. pause the seat, so patrol_scan does not restart it;
//  2. stop its session (a failed stop clears the pause again and does nothing
//     else, the gt-fojqs ordering: a live seat must not stay parked), then
//     remove its sandbox, pushing the commits first: a refused push keeps the
//     sandbox and holds the stop with the seat still paused, the next tick
//     trying again; a sandbox record that does not parse keeps the sandbox and
//     the stop goes on to hand the bead back with a note, the seat left paused
//     (step 6 is skipped);
//  3. checkpoint the worktree;
//  4. push the kept branch to origin; a failed push is retried every tick,
//     and the hand-back waits for it, so its comment never names a branch that
//     exists only in the worktree (see below for when the push is given up);
//  5. end the bead's run (events.TypeOverrunStop) and hand the bead back
//     (status open, assignee cleared, needs-split) so the roster no longer
//     counts the seat; a bead found closed or re-slung to another polecat is
//     left alone. The bead is read once more here, and the run's end is
//     recorded only while it is still this seat's, so the record cannot end a
//     run another polecat has since taken. The end is stamped with the session
//     stop's time and saved, so a stop retried on later ticks (a hand-back that
//     keeps failing) records it once, at the same time, instead of writing a
//     later end every tick that could creep past the dispatch it ends; the
//     dispatch after the hand-back is then a new run the guard polices afresh,
//     and a failed record fails the step, so the hand-back waits for it. When
//     the kept branch's head was already
//     origin's head for it before step 4's push, the checkpoint committed
//     nothing (so the worktree was clean: a commit would have moved the head),
//     and the seat committed that head after the sling began (a seat resumed
//     onto a branch already on origin produced none of it), the finished work
//     is on origin and the label is rework, not needs-split: the comment names
//     the commit and tells the next polecat to reuse the branch and run
//     gt done. Every other stop (a checkpoint that committed or gave up, a
//     push that was refused or given up, no branch on origin, a head older
//     than the sling, a sling time the stop could not date) keeps needs-split.
//     The molecule the bead carried is ended with the hand-back
//     (handBackEndingMolecule), so the run leaves none open. The attention
//     item for the stop is derived from that hand-back
//     (collectOverrunStopped);
//  6. release the seat: reset its agent bead (hook_bead cleared, so the roster
//     stops counting it as dead-hooked), record the cleanup status that reset
//     clears, then retire the seat — end the pause and stop its record in the
//     one write — so the polecat name and its worktree can take new work and
//     nothing reads the sessionless seat as a running seat whose session died.
//     The kept branch is safe without the park: it is on origin and the bead's
//     comment names it. When the push is confirmed — origin's head for the
//     branch, read back with ls-remote, is the worktree's HEAD, and the worktree
//     holds nothing uncommitted — the release also resets the worktree to
//     origin's default branch, detached, and deletes the local branch
//     (resetWorktree), so that once another seat lands the branch the retired
//     seat does not read as unpushed work. A readback that does not confirm
//     leaves the worktree as it is.
//
// Between steps 1 and 2 the save step (overrunSaveWork, 1b) commits the seat's
// uncommitted work as one WIP commit named for the bead and pushes the kept
// branch, once per stop, before the session stops and before the sandbox is
// removed. A refused save holds the stop with the seat paused, and the next
// tick saves again; after overrunSaveMaxAttempts consecutive refusals the stop
// gives up on the save: the sandbox is kept, the bead is handed back with the
// refusal named, and the seat stays parked. An unresolvable worktree is a
// refused save. Before the sandbox is removed (step 2a) the worktree is read
// again: one dirty since the save is saved once more, a refused re-save counts
// toward the same cap, and a dirty worktree that cannot be read holds the stop
// with the sandbox kept.
//
// submittedNow is read before step 1 and before step 4: a seat that has
// submitted (gt done running, or gt:ready-to-land) is not stopped, and a bead
// that gained gt:ready-to-land mid-stop is not handed back. An active e-stop
// (town or rig) is read before step 1 too: a stop that has not begun waits for
// it to lift and parks nothing, since the supervisor would refuse the kill.
//
// A refused checkpoint fails step 3 and is retried once per tick. After
// overrunCheckpointMaxAttempts failures the stop gives up on it: steps 4 and 5
// run (the push keeps what was committed), the comment carrying a note that the
// worktree may hold uncommitted work, and step 6 is skipped, so the seat stays
// paused (and the worktree unreused) until an operator looks. The push has the
// same shape: after overrunPushMaxAttempts failures, or at once when origin
// rejects the unforced push as non-fast-forward and the tip adds work origin
// lacks, the stop gives up on it, and the comment's note says the branch
// exists only in the worktree, names the path and says why the push failed;
// step 6 is skipped. A non-fast-forward refusal whose tip adds nothing origin
// lacks counts as a completed push, so step 6 releases the seat. The
// hand-back's needs-split label raises the attention item; a rework hand-back
// raises none. A resumed stop first checks the pause is still down, before it
// stops the session and again before step 3, where it also checks that no
// session runs on the seat; if not, the seat was taken back and the stop ends
// without a hand-back. Every stop looks again just before step 6, because the
// push and hand-back can outlast an operator resume and a new sling: a seat
// taken back by then keeps the bead handed back, and the release is left to its
// new holder.
//
// Progress is saved after each step, so a step that fails is logged and
// retried from the failed step on the next tick, never from the top and never
// skipping ahead. StoppedAt is written last, and the run the stop ended is not
// stopped again: the guard's skip reads that run's recorded end
// (overrunState.stoppedInsideRun). It deletes a local branch only after origin
// is read back holding that exact commit, never removes a worktree and never
// nukes a seat.
func (d *Daemon) overrunStop(s overrunSeams, seat overrunSeat, st overrunState, elapsed time.Duration) {
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
		st.Rig, st.Polecat, st.Branch, st.Elapsed = seat.Rig, seat.Polecat, seat.Branch, elapsed
		// Saved with the stop's first step, so a stop resumed on a later tick
		// still dates the run its rework decision belongs to.
		st.SlingAt = seat.SlingAt
	}
	// A stop resumed from any saved progress may find the seat taken back, so
	// StopBeganAt decides, not SessionStoppedAt: a failed clearPause or save
	// leaves the stop begun with the session not yet recorded as stopped.
	resumed := !st.StopBeganAt.IsZero()
	branch := st.Branch
	if branch == "" {
		branch = "(unknown)"
	}
	elapsed = st.Elapsed

	// 1. Pause. The seat list is older than this call, so look again first: a
	// seat that has begun gt done since is not stopped.
	if st.StopBeganAt.IsZero() {
		// The e-stop is read before the park. The supervisor refuses the kill
		// under an e-stop, and the refusal clears the park, so parking first
		// would park and un-park the seat on every tick the e-stop is up
		// (gt-m5b31). Nothing is saved here, so the first tick after the
		// e-stop lifts starts the stop from the top.
		if on, err := estop.ActiveFor(d.config.TownRoot, seat.Rig); err != nil || on {
			if err != nil {
				d.logger.Printf("overrun_guard: stop %s on %s waits: cannot read the e-stop: %v", who, st.Bead, err)
			} else {
				d.logger.Printf("overrun_guard: stop %s on %s waits: e-stop active", who, st.Bead)
			}
			return
		}
		if s.submittedNow != nil && s.submittedNow(seat) {
			d.logger.Printf("overrun_guard: %s on %s submitted before the stop; leaving it", who, st.Bead)
			return
		}
		if err := s.pause(seat); err != nil {
			d.logger.Printf("overrun_guard: stop %s on %s: pause failed: %v", who, st.Bead, err)
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
				d.logger.Printf("overrun_guard: stop %s on %s: cannot re-check the seat: %v", who, st.Bead, err)
				return
			}
			if why != "" {
				d.logger.Printf("overrun_guard: %s on %s: %s; abandoning the stop, session left running", who, st.Bead, why)
				// A save that fails is only warned of: the next tick reads the
				// same seat and abandons again, and stops nothing meanwhile.
				st.StoppedAt = now
				save()
				return
			}
		}
		// 1b. Save the seat's work before the session stops and the sandbox
		// goes (overrunSaveWork): uncommitted edits become one WIP commit and
		// the kept branch is pushed. It runs after the re-check above, so a
		// seat taken back is never committed. A refusal holds the stop with the
		// seat paused and the session running, and the next tick saves again,
		// up to overrunSaveMaxAttempts refusals; the cap then parks the seat, and
		// the stop goes on with the sandbox kept and the reason in the note.
		if st.SavedAt.IsZero() && s.saveWork != nil {
			saveSeat := seat
			saveSeat.Bead, saveSeat.Branch, saveSeat.SlingAt = st.Bead, st.Branch, st.SlingAt
			saved, err := s.saveWork(saveSeat)
			if err != nil {
				st.SaveFailures++
				st.SaveFailure = pushFailureReason(err)
				if st.SaveFailures < overrunSaveMaxAttempts {
					d.logger.Printf("overrun_guard: stop %s on %s: work not saved, retrying next tick (%d of %d): %v",
						who, st.Bead, st.SaveFailures, overrunSaveMaxAttempts, err)
					save()
					return
				}
				st.SavedAt, st.SaveGaveUp = now, true
				d.logger.Printf("overrun_guard: stop %s on %s: work not saved after %d attempts, sandbox kept: %v",
					who, st.Bead, st.SaveFailures, err)
			} else {
				st.SavedAt = now
				st.SaveFailure, st.SaveFailures = "", 0
				st.SavedHead, st.SaveCommitted = saved.Head, saved.Committed
			}
			if !save() {
				return
			}
		}
		if err := s.stopSession(seat); err != nil {
			// A refusal is the brakes (an e-stop, or a hold this stop did not
			// write), logged with the supervisor's own text; anything else is
			// a stop that could not run. Both clear the pause: a live seat
			// must not stay parked, and the next tick starts the stop over.
			if errors.Is(err, supervisor.ErrRefused) {
				d.logRefusal(fmt.Sprintf("overrun_guard: stop %s on %s refused", who, st.Bead), err)
			} else {
				d.logger.Printf("overrun_guard: stop %s on %s: session stop failed: %v", who, st.Bead, err)
			}
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
		st.SessionStoppedAt = now
		if !save() {
			return
		}
	}

	// A stop resumed after its session was stopped may find the seat taken
	// back: an operator lifted the pause, or a new session started on it.
	// Steps 3 to 5 then act on a bead that session holds, so the stop ends.
	if resumed && s.stopLapsed != nil {
		why, err := s.stopLapsed(seat, true)
		if err != nil {
			d.logger.Printf("overrun_guard: stop %s on %s: cannot re-check the seat: %v", who, st.Bead, err)
			return
		}
		if why != "" {
			// StoppedAt ends the stop for this sling; the bead stays with its holder.
			st.StoppedAt = now
			save()
			d.logger.Printf("overrun_guard: %s on %s: %s; abandoning the stop, bead not handed back", who, st.Bead, why)
			return
		}
	}

	// 2b. Remove the sandbox a sandboxed seat ran in, pushing its commits first.
	// It runs after the resumed-stop check above, so a seat taken back is never
	// swept. A refused push keeps the sandbox and its record, and the stop waits
	// here with the pause still held, so nothing restarts the seat while its
	// commits are unpushed. A record that does not parse cannot say whether the
	// seat ran in a sandbox, and no retry changes the file: the sandbox is left
	// as it is, and the stop goes on to hand the bead back with a note, the
	// seat left paused (sandboxKept). A host seat has no record and this does
	// nothing; a seat on a rig the registry does not list is found by its
	// record's rig and polecat, and a lookup that cannot tell holds the stop.
	// A save that gave up (SaveGaveUp, step 1b or 2a) keeps the sandbox too: its
	// work is not known to be on origin, so nothing removes it. A refusal short
	// of the cap holds the stop before the sandbox is touched.
	saveKept := st.SaveGaveUp
	sandboxKept := false
	if s.removeSandbox != nil && !saveKept {
		// 2a. Re-check the worktree before the sandbox goes. The save (1b) ran
		// before the session stop, so an edit made between the two is not in
		// it: a worktree dirty again is saved once more. A dirty check that
		// fails holds the stop with the sandbox kept, as a refused push does, and
		// the next tick checks again. A refused re-save holds the same way, up to
		// overrunSaveMaxAttempts refusals; the cap parks the seat and keeps the
		// sandbox, as a refused save at 1b does.
		if s.saveWork != nil {
			dirty, err := d.overrunWorktreeDirty(seat)
			if err != nil {
				d.logger.Printf("overrun_guard: stop %s on %s waits, sandbox kept: %v", who, st.Bead, err)
				return
			}
			// A refused save may have committed without pushing (the WIP commit
			// stays in the worktree), so a pending refusal saves again even when
			// the worktree now reads clean: the sandbox never goes on an unpushed
			// commit.
			if dirty || st.SaveFailure != "" {
				saveSeat := seat
				saveSeat.Bead, saveSeat.Branch, saveSeat.SlingAt = st.Bead, st.Branch, st.SlingAt
				saved, err := s.saveWork(saveSeat)
				if err != nil {
					st.SaveFailures++
					st.SaveFailure = pushFailureReason(err)
					if st.SaveFailures < overrunSaveMaxAttempts {
						d.logger.Printf("overrun_guard: stop %s on %s waits: work edited after the save not saved, retrying next tick (%d of %d), sandbox kept: %v",
							who, st.Bead, st.SaveFailures, overrunSaveMaxAttempts, err)
						save()
						return
					}
					st.SaveGaveUp, saveKept = true, true
					d.logger.Printf("overrun_guard: stop %s on %s: work edited after the save not saved after %d attempts, sandbox kept: %v",
						who, st.Bead, st.SaveFailures, err)
				} else {
					st.SaveFailure, st.SaveFailures = "", 0
					if saved.Head != "" {
						st.SavedHead, st.SaveCommitted = saved.Head, saved.Committed
					}
				}
				if !save() {
					return
				}
			}
		}
	}
	if s.removeSandbox != nil && !saveKept {
		err := s.removeSandbox(seat)
		switch {
		case err == nil:
		case errors.Is(err, errSandboxRecordCorrupt):
			sandboxKept = true
			d.logger.Printf("overrun_guard: WARNING: stop %s on %s: sandbox record unreadable, sandbox kept; handing the bead back with a note: %v", who, st.Bead, err)
		default:
			d.logger.Printf("overrun_guard: stop %s on %s waits, sandbox kept: %v", who, st.Bead, err)
			return
		}
	}

	// 3. Checkpoint the worktree.
	if st.CheckpointedAt.IsZero() && !st.CheckpointGaveUp {
		if err := s.checkpoint(seat); err != nil {
			st.CheckpointFailures++
			d.logger.Printf("overrun_guard: stop %s on %s: checkpoint failed (%d of %d): %v",
				who, st.Bead, st.CheckpointFailures, overrunCheckpointMaxAttempts, err)
			if st.CheckpointFailures < overrunCheckpointMaxAttempts {
				save()
				return
			}
			// The refusal has outlasted every retry and nothing changes the
			// worktree while the seat is stopped: hand the bead back without it.
			st.CheckpointGaveUp = true
			if !save() {
				return
			}
			d.logger.Printf("overrun_guard: stop %s on %s: giving up on the checkpoint; handing the bead back without it",
				who, st.Bead)
		} else {
			st.CheckpointedAt = now
			if !save() {
				return
			}
		}
	}

	// Steps 4 and 5 run unless the work was submitted while the stop ran: a
	// bead that is gt:ready-to-land must not also carry needs-split. The seat
	// is stopped and its work checkpointed, so release the pause and finish.
	if s.submittedNow != nil && s.submittedNow(seat) {
		if err := s.clearPause(seat); err != nil {
			d.logger.Printf("overrun_guard: WARNING: %s: pause marker not cleared: %v", who, err)
			return
		}
		st.StoppedAt = now
		save()
		d.logger.Printf("overrun_guard: %s on %s submitted during the stop; not handed back", who, st.Bead)
		return
	}

	// 4. Push the kept branch, so the comment below never names a branch that
	// exists only in the worktree and the seat reads recoverable. A failed
	// push is retried every tick: the hand-back waits for it, up to
	// overrunPushMaxAttempts; a non-fast-forward rejection, which no retry
	// changes, gives up at once.
	if st.PushedAt.IsZero() && !st.PushGaveUp && s.push != nil {
		pushSeat := seat
		pushSeat.Bead, pushSeat.Branch, pushSeat.SlingAt = st.Bead, st.Branch, st.SlingAt
		// Read before the push: afterwards origin holds the head whatever it
		// held before. A checkpoint that gave up leaves work in the worktree,
		// which is no finished branch. The answer is saved before the push, so
		// a save that fails afterwards cannot lose it: the retry pushes again,
		// but keeps the decision instead of reading an origin the stop's own
		// push has already changed.
		if st.ReworkDecidedAt.IsZero() {
			onOrigin := ""
			if s.finishedOnOrigin != nil && !st.CheckpointGaveUp {
				onOrigin = s.finishedOnOrigin(pushSeat)
			}
			st.ReworkSHA, st.ReworkDecidedAt = onOrigin, now
			if !save() {
				return
			}
		}
		if err := s.push(pushSeat); err != nil {
			st.PushFailures++
			st.PushFailure = pushFailureReason(err)
			var diverged *overrunPushDivergedError
			if !errors.As(err, &diverged) && st.PushFailures < overrunPushMaxAttempts {
				d.logger.Printf("overrun_guard: stop %s on %s: push of branch %s failed, retrying next tick: %v (%d of %d)",
					who, st.Bead, branch, err, st.PushFailures, overrunPushMaxAttempts)
				save()
				return
			}
			// The push will not succeed while the seat is stopped: hand the
			// bead back with the branch named as living in the worktree only.
			st.PushGaveUp = true
			if !save() {
				return
			}
			d.logger.Printf("overrun_guard: stop %s on %s: giving up on the push of branch %s after %d attempt(s); handing the bead back with a note: %v",
				who, st.Bead, branch, st.PushFailures, err)
		} else {
			st.PushedAt = now
			if !save() {
				return
			}
		}
	}

	// 5. End the bead's run, then hand it back. The stop is what ended the
	// run, so it is recorded on the bead before the hand-back: the dispatch
	// after it is a new run, and the guard polices that run afresh instead of
	// reading it as this one, already stopped, and leaving it alone until a
	// done event the hand-back never writes. The record is written while the
	// bead is still hooked to the stopped seat, so no re-sling can land before
	// it. A failed write fails the step and the hand-back waits for the next
	// tick: failing open here would leave the handed-back bead unpoliced.
	handed := seat
	handed.Bead, handed.Branch = st.Bead, branch
	if !st.CheckpointGaveUp && !st.PushGaveUp && !sandboxKept && !saveKept {
		handed.ReworkSHA = st.ReworkSHA
	}
	var notes []string
	if st.SavedHead != "" {
		if st.SaveCommitted {
			notes = append(notes, fmt.Sprintf("The seat's uncommitted work was saved before the stop as a WIP commit, and branch %s was pushed at %s.",
				branch, st.SavedHead))
		} else {
			notes = append(notes, fmt.Sprintf("Branch %s was pushed at %s before the stop.", branch, st.SavedHead))
		}
	}
	if saveKept {
		notes = append(notes, fmt.Sprintf("The seat's work could not be saved before the stop (%s): branch %s may hold work origin lacks. "+
			"The sandbox is kept as it was, and %s stays paused until an operator checks it and runs gt agent resume.",
			st.SaveFailure, branch, who))
	}
	if sandboxKept {
		notes = append(notes, fmt.Sprintf("The seat's sandbox could not be removed: its record is unreadable, so it may hold commits that were never pushed. "+
			"The sandbox is kept as it was, and %s stays paused until an operator checks it and runs gt agent resume.", who))
	}
	if st.CheckpointGaveUp {
		notes = append(notes, fmt.Sprintf("The worktree could not be checkpointed after %d attempts and may hold uncommitted work; "+
			"it is left as it was, and %s stays paused until an operator checks it and runs gt agent resume.",
			st.CheckpointFailures, who))
	}
	if st.PushGaveUp {
		notes = append(notes, fmt.Sprintf("Branch %s exists only in the seat's worktree (%s): it could not be pushed to origin (%s); "+
			"%s stays paused until an operator pushes it and runs gt agent resume.",
			branch, d.overrunWorktreePath(seat), st.PushFailure, who))
	}
	// The run's end is recorded once per stop, and only while the bead is still
	// the stopped seat's. Both matter on a retry: a hand-back that keeps failing
	// runs this step every tick, and a record written under a later time each
	// time would creep past the dispatch it ends, leaving the bead dated from
	// the agent bead's write (which reads short). The time is the session stop's,
	// saved with the stop, so a repeated record carries the same end; and a bead
	// that closed or moved to another seat in between belongs to that holder, so
	// ending its run would date the new holder's clock by the old run.
	if s.endRun != nil && st.RunEndedAt.IsZero() {
		ours := true
		if s.stillOurs != nil {
			var err error
			ours, err = s.stillOurs(handed)
			if err != nil {
				d.logger.Printf("overrun_guard: stop %s on %s: cannot read the bead before ending its run: %v",
					who, st.Bead, err)
				return
			}
		}
		if ours {
			at := st.SessionStoppedAt
			if at.IsZero() {
				at = now
			}
			if err := s.endRun(handed, at); err != nil {
				d.logger.Printf("overrun_guard: stop %s on %s: the run's end could not be recorded, so the bead is not handed back yet: %v",
					who, st.Bead, err)
				return
			}
			st.RunEndedAt = at
			if !save() {
				return
			}
		} else {
			d.logger.Printf("overrun_guard: stop %s on %s: the bead is no longer this seat's; not ending its run", who, st.Bead)
		}
	}
	if err := s.handBack(handed, elapsed, strings.Join(notes, " ")); err != nil {
		d.logger.Printf("overrun_guard: stop %s on %s: hand-back failed: %v", who, st.Bead, err)
		return
	}
	// The run ended when the session was stopped, so the attempt is closed at
	// that time: the stop is what ended it, and the bead is handed back above.
	// An attempt left open would read as unfinished work in the bead's time
	// (gt-dz78b). It is a measurement, not a step: a failed write is logged and
	// the rest of the stop goes on.
	if s.spanEnd != nil {
		at := st.SessionStoppedAt
		if at.IsZero() {
			at = now
		}
		if err := s.spanEnd(handed, at); err != nil {
			d.logger.Printf("overrun_guard: WARNING: %s: span not closed for %s: %v", who, st.Bead, err)
		}
	}
	if st.CheckpointGaveUp || st.PushGaveUp || sandboxKept || saveKept {
		// 6 is skipped: the park is what keeps the name, and the worktree
		// under it, from taking new work while it holds state origin lacks
		// (or, for a kept sandbox, while its commits are unknown).
		st.StoppedAt = now
		if !save() {
			d.logger.Printf("overrun_guard: WARNING: %s: %s handed back but state not saved — the hand-back will be repeated", who, st.Bead)
			return
		}
		d.logger.Printf("overrun_guard: stopped %s on %s after %s; bead handed back with a note (branch %s and worktree kept, seat left paused)",
			who, st.Bead, elapsed.Round(time.Second), branch)
		return
	}
	// The push and the hand-back are the slow steps, and the seat can be taken
	// back during them: an operator resumes it and a new sling lands on the
	// name. Step 6 writes the seat's agent bead, cleanup status and intent
	// record, which would then be that sling's, so look again first. The bead
	// is already handed back; only the release is left to its new holder.
	if s.stopLapsed != nil {
		why, err := s.stopLapsed(seat, true)
		if err != nil {
			d.logger.Printf("overrun_guard: stop %s on %s: cannot re-check the seat before releasing it: %v", who, st.Bead, err)
			return
		}
		if why != "" {
			st.StoppedAt = now
			save()
			d.logger.Printf("overrun_guard: %s on %s: %s; bead handed back, seat not released", who, st.Bead, why)
			return
		}
	}

	// 6. Release the seat. The bead no longer names it, so nothing restarts it
	// on this sling; leaving the pause would make parkedReuseBlocker refuse the
	// name until an operator ran gt agent resume, with no notice that it was
	// parked. A failed step leaves the stop pending, so the next tick repeats
	// the hand-back (HandBack finishes a bead it already released) and the
	// release. Every step runs while the pause still keeps a new sling off the
	// name: one that still names the handed-back bead reads as a dead-hooked
	// seat to the pool roster, and a seat retired out from under a new sling
	// would lose that sling's record.
	if s.resetAgentBead != nil {
		reset := seat
		reset.Bead = st.Bead
		if err := s.resetAgentBead(reset); err != nil {
			d.logger.Printf("overrun_guard: WARNING: %s: agent bead not reset after the hand-back of %s: %v", who, st.Bead, err)
			return
		}
	}
	// A confirmed push leaves nothing in the worktree that origin lacks, so the
	// seat goes idle before its cleanup status is probed. PushedAt is set only
	// by a push this stop made; a give-up returned above. The reset rechecks the
	// push itself, so a branch origin lacks leaves the worktree alone. A failure
	// is logged, not retried, since the branch is already on origin.
	if s.resetWorktree != nil && !st.PushedAt.IsZero() {
		reset := seat
		reset.Bead, reset.Branch = st.Bead, st.Branch
		if err := s.resetWorktree(reset); err != nil {
			d.logger.Printf("overrun_guard: WARNING: %s: worktree not reset after the push of %s: %v", who, branch, err)
		}
	}
	// The cleanup status the reset above cleared is recorded next, while the
	// pause still keeps a new sling off the name: without it a later
	// `gt polecat nuke` refuses the seat it was told to release, for a
	// cleanup_status=<missing> the release itself created (gt-6ixtp). A probe
	// that cannot read the worktree records unknown, and a failed write is
	// logged rather than retried: it holds no work back.
	if s.recordCleanup != nil {
		if err := s.recordCleanup(seat); err != nil {
			d.logger.Printf("overrun_guard: WARNING: %s: cleanup status not recorded for the released seat %s: %v", who, st.Bead, err)
		}
	}
	// Retire last: it ends the hold and stops the seat's record in one write,
	// so no reader sees a running seat whose session is gone (gt-6ixtp). A
	// failure leaves the hold standing, so the next tick retries the release,
	// and the seat stays parked (and named by nobody) until it works.
	if s.retire != nil {
		if err := s.retire(seat); err != nil {
			d.logger.Printf("overrun_guard: WARNING: %s: seat not retired after the hand-back of %s: %v", who, st.Bead, err)
			return
		}
	}
	st.StoppedAt = now
	if !save() {
		d.logger.Printf("overrun_guard: WARNING: %s: %s handed back but state not saved — the hand-back will be repeated", who, st.Bead)
		return
	}
	d.logger.Printf("overrun_guard: stopped %s on %s after %s; bead handed back (branch %s kept)",
		who, st.Bead, elapsed.Round(time.Second), branch)
}

// resumeOverrunStops finishes the stops that began on an earlier tick and did
// not complete. It returns the beads it touched, so the same tick does not
// start them again from the seat list.
func (d *Daemon) resumeOverrunStops(s overrunSeams) map[string]bool {
	touched := map[string]bool{}
	for _, st := range pendingOverrunStops(d.config.TownRoot) {
		if st.Rig == "" || st.Polecat == "" {
			continue
		}
		touched[st.Bead] = true
		seat := overrunSeat{Rig: st.Rig, Polecat: st.Polecat, Bead: st.Bead, Branch: st.Branch,
			Scheduled: st.Scheduled, SlingAt: st.SlingAt}
		if st.Scheduled {
			d.overrunScheduledStop(s, seat, st, st.Elapsed)
			continue
		}
		d.overrunStop(s, seat, st, st.Elapsed)
	}
	return touched
}

// overrunStopSeams fills the stop's side effects with the production ones.
func (d *Daemon) overrunStopSeams(s overrunSeams) overrunSeams {
	s.pause = func(seat overrunSeat) error {
		reason := overrunPauseReason
		if seat.Scheduled {
			reason = overrunScheduledPauseReason
		}
		return agentpause.Pause(d.config.TownRoot, seat.Rig, constants.RolePolecat, seat.Polecat,
			reason, overrunActor, "")
	}
	s.comment = d.overrunCommentBead
	s.closeBead = d.overrunCloseRunBead
	s.clearPause = func(seat overrunSeat) error {
		return agentpause.Resume(d.config.TownRoot, seat.Rig, constants.RolePolecat, seat.Polecat)
	}
	s.stopSession = d.overrunStopSession
	s.saveWork = d.overrunSaveWork
	s.removeSandbox = func(seat overrunSeat) error { return d.overrunRemoveSandbox(seat, nil, nil) }
	s.submittedNow = d.overrunSubmittedNow
	s.checkpoint = func(seat overrunSeat) error {
		polecatsDir := filepath.Join(d.config.TownRoot, seat.Rig, "polecats")
		workDir := resolvePolecatWorktree(polecatsDir, seat.Polecat, seat.Rig)
		if workDir == "" {
			// Nothing to checkpoint; the branch is whatever it already holds.
			d.logger.Printf("overrun_guard: %s/%s: no worktree to checkpoint", seat.Rig, seat.Polecat)
			return nil
		}
		// A refused checkpoint fails the step: the hand-back comment names the
		// branch as holding the work, which dirty files alone do not.
		if d.checkpointWorktreeOutcome(workDir, seat.Rig, seat.Polecat) == checkpointRefused {
			return errors.New("checkpoint refused; uncommitted work may remain in the worktree")
		}
		return nil
	}
	s.stopLapsed = d.overrunStopLapsed
	s.push = d.overrunPushBranch
	s.finishedOnOrigin = d.overrunFinishedOnOrigin
	s.resetAgentBead = d.overrunResetAgentBead
	s.recordCleanup = d.overrunRecordCleanupStatus
	s.resetWorktree = d.overrunResetWorktree
	s.retire = func(seat overrunSeat) error {
		return agentpause.Retire(d.config.TownRoot, seat.Rig, constants.RolePolecat, seat.Polecat, overrunActor)
	}
	s.spanEnd = func(seat overrunSeat, at time.Time) error {
		// A seat with no open attempt (its sling never reached the store, or
		// gt done ended it) has nothing to close.
		_, err := spans.New(spans.DefaultDir(d.config.TownRoot), nil).
			EndOpen(seat.Bead, seat.Rig+"/"+seat.Polecat, spans.ReasonOverrun, at)
		return err
	}
	s.stillOurs = func(seat overrunSeat) (bool, error) {
		// The same read the hand-back makes before it writes, so the run's end
		// and the hand-back agree on whose bead it is: a closed bead, or one
		// another polecat holds now, is that holder's (gt-tzbtf).
		ctx, cancel := context.WithTimeout(context.Background(), overrunHandBackTimeout)
		defer cancel()
		var store overrunHandBackStore = beads.NewPlain(d.config.TownRoot, bdMutationRoutingEnv(d.config.TownRoot),
			beads.WithBin(d.bdPathOrDefault())).WithTimeout(overrunHandBackTimeout)
		if d.openOverrunBeads != nil {
			store = d.openOverrunBeads()
		}
		status, assignee, err := polecat.NewBeadWriter(store, overrunActor).State(ctx, seat.Bead)
		if err != nil {
			return false, fmt.Errorf("read bead %s: %w", seat.Bead, err)
		}
		// An empty assignee is the half-released bead this stop already began
		// handing back, and it is finished like one the seat still holds.
		return overrunSeatOwnsBead(seat, status, assignee), nil
	}
	s.endRun = func(seat overrunSeat, at time.Time) error {
		// The town's event log is the clock both the guard and the dashboard
		// read (events.DispatchStart), so the run's end is recorded there and
		// nowhere else (gt-tzbtf). "at" is the run's end as the stop saved it,
		// so a record written by a retry dates the boundary the same way.
		return events.LogFeedTo(d.config.TownRoot, events.TypeOverrunStop, overrunActor,
			map[string]interface{}{"bead": seat.Bead, "at": at.UTC().Format(time.RFC3339)})
	}
	s.handBack = func(seat overrunSeat, elapsed time.Duration, note string) error {
		ctx, cancel := context.WithTimeout(context.Background(), overrunHandBackTimeout)
		defer cancel()
		store := beads.NewPlain(d.config.TownRoot, bdMutationRoutingEnv(d.config.TownRoot),
			beads.WithBin(d.bdPathOrDefault())).WithTimeout(overrunHandBackTimeout)
		err := handBackEndingMolecule(ctx, store, d.logger.Printf, seat, elapsed, note)
		if errors.Is(err, polecat.ErrHandBackSkipped) {
			// The bead is no longer this seat's: nothing to release, nothing to repeat.
			d.logger.Printf("overrun_guard: %s/%s: %v", seat.Rig, seat.Polecat, err)
			return nil
		}
		return err
	}
	return s
}

// overrunSeatAssignee is the assignee bd stores for a bead the seat holds.
func overrunSeatAssignee(seat overrunSeat) string {
	return seat.Rig + "/polecats/" + seat.Polecat
}

// overrunSeatOwnsBead reports whether a bead in this status and assignee is
// still the seat's to end: a closed or tombstoned bead is not, and an
// unassigned bead or one assigned to the seat is.
func overrunSeatOwnsBead(seat overrunSeat, status, assignee string) bool {
	if status == "closed" || status == "tombstone" {
		return false
	}
	return assignee == "" || assignee == overrunSeatAssignee(seat)
}

// overrunPushBranch pushes the seat's kept branch to origin at the commit the
// local branch holds (git push origin <sha>:refs/heads/<branch>), never forced.
// A seat with no worktree or no branch has nothing to push; a branch that
// cannot be read or pushed is an error, so the stop retries it. A push origin
// refuses as non-fast-forward succeeds when the tip adds nothing origin lacks:
// such a push had nothing left to send.
func (d *Daemon) overrunPushBranch(seat overrunSeat) error {
	polecatsDir := filepath.Join(d.config.TownRoot, seat.Rig, "polecats")
	workDir := resolvePolecatWorktree(polecatsDir, seat.Polecat, seat.Rig)
	if workDir == "" {
		d.logger.Printf("overrun_guard: %s/%s: no worktree to push", seat.Rig, seat.Polecat)
		return nil
	}
	g := d.gitAt(workDir)
	branch := seat.Branch
	if branch == "" {
		current, err := g.CurrentBranch()
		if err != nil {
			return fmt.Errorf("read branch: %w", err)
		}
		branch = current
	}
	if branch == "" || branch == "HEAD" {
		d.logger.Printf("overrun_guard: %s/%s: no branch to push", seat.Rig, seat.Polecat)
		return nil
	}
	sha, err := g.Rev("refs/heads/" + branch)
	if err != nil {
		return fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	if err := g.Push("origin", sha+":refs/heads/"+branch, false); err != nil {
		if isNonFastForwardPush(err) {
			originSHA := originBranchSHA(g, branch)
			// A tip that adds no patch origin lacks has nothing left to send, so
			// releasing the seat loses no work; a tip that does add one keeps the
			// seat paused (gt-ao7zh).
			if overrunTipAddsNothing(g, sha, originSHA) {
				d.logger.Printf("overrun_guard: %s/%s: origin refused the push of %s as non-fast-forward, but its tip %s adds nothing origin lacks; treating the push as done",
					seat.Rig, seat.Polecat, branch, sha)
				return nil
			}
			return &overrunPushDivergedError{Branch: branch, OriginSHA: originSHA, Err: err}
		}
		return fmt.Errorf("push %s: %w", branch, err)
	}
	return nil
}

// overrunSave is what the save step (overrunSaveWork) left on origin: the
// branch head it pushed, and whether that head is a WIP commit it made.
type overrunSave struct {
	Head      string
	Committed bool
}

// overrunSaveMessage is the subject of the WIP commit the save step makes.
func overrunSaveMessage(bead string) string {
	return "WIP overrun stop: " + bead
}

// overrunSaveWork is the saveWork seam. A worktree's uncommitted work becomes
// one WIP commit, named for the bead, through the checkpoint's staging and
// guards, so the runtime files and throwaway paths the checkpoint leaves out
// stay out here too; the kept branch is then pushed, unforced, when origin
// does not hold its head. A worktree that is clean and already pushed changes
// nothing. A worktree that cannot be resolved is an error, never nothing to
// save. A WIP commit made on no branch, or on a HEAD that is not the kept
// branch, cannot reach that branch, so it is refused rather than left
// unpushed. A refused commit or push is an error, and the stop keeps the
// sandbox.
func (d *Daemon) overrunSaveWork(seat overrunSeat) (overrunSave, error) {
	workDir, err := d.overrunSeatWorktree(seat)
	if err != nil {
		return overrunSave{}, err
	}
	committed := false
	switch d.checkpointWorktreeAs(workDir, seat.Rig, seat.Polecat, overrunSaveMessage(seat.Bead)) {
	case checkpointCreated:
		committed = true
	case checkpointRefused:
		return overrunSave{}, errors.New("uncommitted work could not be committed; it may remain in the worktree")
	}
	g := d.gitAt(workDir)
	branch := seat.Branch
	if branch == "" {
		current, err := g.CurrentBranch()
		if err != nil {
			return overrunSave{}, fmt.Errorf("read branch: %w", err)
		}
		branch = current
	}
	if branch == "" || branch == "HEAD" {
		if committed {
			return overrunSave{}, errors.New("the worktree is on no branch, so its WIP commit cannot be pushed")
		}
		return overrunSave{}, nil
	}
	sha, err := g.Rev("refs/heads/" + branch)
	if err != nil {
		return overrunSave{}, fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	head, headErr := g.Rev("HEAD")
	if committed {
		// The WIP commit landed on HEAD: it reaches the branch only if HEAD is it.
		if headErr != nil || head != sha {
			return overrunSave{}, fmt.Errorf("HEAD is not on branch %s, so the WIP commit cannot be pushed to it", branch)
		}
	} else if headErr != nil {
		return overrunSave{}, fmt.Errorf("read HEAD: %w", headErr)
	} else if head != sha {
		// A clean worktree whose HEAD is off the kept branch (a detached HEAD, or
		// another branch) is saved only if the branch already holds HEAD's
		// commits. Comparing the branch ref alone would call it saved while
		// commits reachable only from HEAD are lost with the sandbox.
		if held, err := g.IsAncestor(head, sha); err != nil || !held {
			return overrunSave{}, fmt.Errorf("HEAD holds commits branch %s does not, so they would be lost with the sandbox", branch)
		}
	}
	if !committed && originBranchSHA(g, branch) == sha {
		return overrunSave{}, nil
	}
	pushSeat := seat
	pushSeat.Branch = branch
	if err := d.overrunPushBranch(pushSeat); err != nil {
		return overrunSave{}, err
	}
	return overrunSave{Head: sha, Committed: committed}, nil
}

// overrunSeatWorktree is the seat's worktree. One that cannot be resolved is an
// error, never an empty answer: the save and the dirty check cannot see a
// worktree they cannot resolve, and that worktree may hold the seat's work.
func (d *Daemon) overrunSeatWorktree(seat overrunSeat) (string, error) {
	polecatsDir := filepath.Join(d.config.TownRoot, seat.Rig, "polecats")
	workDir := resolvePolecatWorktree(polecatsDir, seat.Polecat, seat.Rig)
	if workDir == "" {
		return "", fmt.Errorf("worktree of %s/%s cannot be resolved, so its work cannot be saved", seat.Rig, seat.Polecat)
	}
	return workDir, nil
}

// overrunWorktreeDirty reports whether the seat's worktree holds uncommitted
// work now. A status that cannot be read is an error, never clean.
func (d *Daemon) overrunWorktreeDirty(seat overrunSeat) (bool, error) {
	workDir, err := d.overrunSeatWorktree(seat)
	if err != nil {
		return false, err
	}
	status, err := d.gitAt(workDir).Status()
	if err != nil {
		return false, fmt.Errorf("read status of %s/%s: %w", seat.Rig, seat.Polecat, err)
	}
	return !status.Clean, nil
}

// overrunTipAddsNothing reports whether tip adds no patch origin lacks: its
// commits are already in the commit origin holds now for the branch, or in the
// commit origin holds now for its default branch. Both come from ls-remote, so
// a stale remote-tracking ref cannot pass for origin's content. Only a positive
// answer counts — an origin that cannot be asked, or a history git cannot
// compare, answers false, and the seat stays paused rather than released over
// work that may exist only in the worktree.
func overrunTipAddsNothing(g daemonGit, tip, originSHA string) bool {
	var upstreams []string
	if originSHA != "" {
		upstreams = append(upstreams, originSHA)
	}
	if defaultBranch := g.RemoteDefaultBranch(); defaultBranch != "" {
		if sha := originBranchSHA(g, defaultBranch); sha != "" {
			upstreams = append(upstreams, sha)
		}
	}
	for _, upstream := range upstreams {
		if overrunTipInHistory(g, tip, upstream) {
			return true
		}
	}
	return false
}

// overrunTipInHistory reports whether every commit tip carries is already in
// upstream, by ancestry first and then by patch identity (git cherry's "-"
// lines mark a patch upstream already has). A comparison git cannot make is
// not a match.
func overrunTipInHistory(g daemonGit, tip, upstream string) bool {
	if tip == upstream {
		return true
	}
	if ancestor, err := g.IsAncestor(tip, upstream); err == nil && ancestor {
		return true
	}
	out, err := g.Cherry(upstream, tip)
	if err != nil {
		return false
	}
	return git.CountCherryUnmergedCommits(out) == 0
}

// overrunResetWorktree is the resetWorktree seam: it puts a released seat's
// worktree back to its idle state, detached at origin's default branch with the
// kept branch deleted, once the push is confirmed. Confirmed means origin, asked
// now with ls-remote, holds for the branch exactly the commit the worktree's
// HEAD is on, and the worktree holds nothing uncommitted and no stash. Anything
// else — no worktree or branch, a branch already gone, origin unreachable or
// holding another commit, HEAD off the branch, work at risk — leaves the
// worktree untouched and returns nil: the push step already stands for the
// branch, and the seat reads as it did before this reset existed. The delete is
// forced because the branch is no longer merged into the HEAD the detach
// leaves; origin holding its tip is what makes that safe.
func (d *Daemon) overrunResetWorktree(seat overrunSeat) error {
	who := seat.Rig + "/" + seat.Polecat
	polecatsDir := filepath.Join(d.config.TownRoot, seat.Rig, "polecats")
	workDir := resolvePolecatWorktree(polecatsDir, seat.Polecat, seat.Rig)
	if workDir == "" {
		return nil
	}
	g := d.gitAt(workDir)
	branch := seat.Branch
	if branch == "" || branch == "(unknown)" {
		current, err := g.CurrentBranch()
		if err != nil {
			return nil
		}
		branch = current
	}
	if branch == "" || branch == "HEAD" {
		return nil
	}
	tip, err := g.Rev("refs/heads/" + branch)
	if err != nil || tip == "" {
		return nil
	}
	if head, err := g.Rev("HEAD"); err != nil || head != tip {
		d.logger.Printf("overrun_guard: %s: HEAD is not on branch %s; worktree left as it is", who, branch)
		return nil
	}
	if origin := originBranchSHA(g, branch); origin != tip {
		d.logger.Printf("overrun_guard: %s: origin does not hold %s at %s; worktree left as it is", who, branch, tip)
		return nil
	}
	if status := overrunCleanupStatus(g); status != string(polecat.CleanupClean) {
		d.logger.Printf("overrun_guard: %s: worktree is %s; left as it is", who, status)
		return nil
	}
	base := g.CleanDefaultBranchBaseRef("origin", g.RemoteDefaultBranch())
	if err := g.CheckoutDetach(base); err != nil {
		return fmt.Errorf("detach at %s: %w", base, err)
	}
	if err := g.DeleteBranch(branch, true); err != nil {
		return fmt.Errorf("delete branch %s: %w", branch, err)
	}
	d.logger.Printf("overrun_guard: %s: worktree reset to %s, branch %s dropped (origin holds %s)", who, base, branch, tip)
	return nil
}

// overrunFinishedOnOrigin is the finishedOnOrigin seam: the commit the seat's
// kept branch holds in its worktree when origin holds that same commit for the
// branch and the seat committed it after the bead's dispatch clock began, ""
// otherwise. That check is what keeps a rework bead, slung onto the branch the
// landing worker rejected, from reading as finished work: the rejected landing
// submitted the bead, so the rework dispatch is a new run's clock, and the head
// the seat resumed predates it — the seat has done nothing since, and the stop
// is an overrun (needs-split, with its attention item) rather than rework.
//
// The check reads the head's committer time, not its author time: the author
// date can be set freely at commit time, while the committer date belongs to
// the operation that wrote the commit. A seat that rebases or cherry-picks the
// branch rewrites the head and stamps it with the moment of that rewrite, so
// the new head counts as produced by the run — which it is. A head merely
// resumed keeps the committer time it was pushed with.
//
// A seat with no worktree or branch, a branch origin lacks, an origin that
// cannot be asked, a head whose committer time cannot be read, and a seat
// whose sling time is unknown (a stop resumed from a record saved before the
// sling was kept) all answer "": with nothing showing the head was produced
// during the run, the stop takes the needs-split path, whose attention item
// asks for the operator look.
func (d *Daemon) overrunFinishedOnOrigin(seat overrunSeat) string {
	polecatsDir := filepath.Join(d.config.TownRoot, seat.Rig, "polecats")
	workDir := resolvePolecatWorktree(polecatsDir, seat.Polecat, seat.Rig)
	if workDir == "" {
		return ""
	}
	g := d.gitAt(workDir)
	branch := seat.Branch
	if branch == "" {
		current, err := g.CurrentBranch()
		if err != nil {
			return ""
		}
		branch = current
	}
	if branch == "" || branch == "HEAD" {
		return ""
	}
	sha, err := g.Rev("refs/heads/" + branch)
	if err != nil || sha == "" {
		return ""
	}
	if originBranchSHA(g, branch) != sha {
		return ""
	}
	// A head committed before the sling is the branch the seat resumed, not
	// work this run produced. An unknown sling time cannot tell the two apart,
	// so it answers "" like an old head: needs-split is the safe hand-back.
	if seat.SlingAt.IsZero() {
		return ""
	}
	at, err := g.CommitTime(sha)
	if err != nil || !at.After(seat.SlingAt) {
		return ""
	}
	return sha
}

// overrunPushDivergedError is a push origin rejected because it holds history
// for the branch that the local branch does not contain. No retry changes
// that, and the push is never forced, so the stop gives up on it at once.
type overrunPushDivergedError struct {
	Branch    string
	OriginSHA string // empty when origin could not be asked
	Err       error
}

func (e *overrunPushDivergedError) Error() string {
	return fmt.Sprintf("push %s: %s: %v", e.Branch, e.reason(), e.Err)
}

func (e *overrunPushDivergedError) Unwrap() error { return e.Err }

// reason is the hand-back note's account of the rejection.
func (e *overrunPushDivergedError) reason() string {
	if e.OriginSHA == "" {
		return "origin holds a different history for this branch (origin sha unknown)"
	}
	return "origin holds a different history for this branch (origin sha " + e.OriginSHA + ")"
}

// isNonFastForwardPush reports whether err is git's non-fast-forward
// rejection, worded "non-fast-forward" when git knows the remote ref's history
// and "fetch first" when it knows only that the ref moved.
func isNonFastForwardPush(err error) bool {
	var ge *git.GitError
	if !errors.As(err, &ge) {
		return false
	}
	return strings.Contains(ge.Stderr, "non-fast-forward") || strings.Contains(ge.Stderr, "fetch first")
}

// originBranchSHA is the commit origin holds for branch, or "" when origin
// cannot be asked or does not have it.
func originBranchSHA(g daemonGit, branch string) string {
	ref := "refs/heads/" + branch
	refs, err := g.ListRemoteRefsWithHashes("origin", ref)
	if err != nil {
		return ""
	}
	for _, r := range refs {
		if r.Name == ref {
			return r.Hash
		}
	}
	return ""
}

// pushFailureReason is why a push failed, on one line and bounded, for the
// hand-back note and the saved state.
func pushFailureReason(err error) string {
	var diverged *overrunPushDivergedError
	if errors.As(err, &diverged) {
		return diverged.reason()
	}
	const maxLen = 300
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "..."
	}
	return msg
}

// overrunWorktreePath is where the seat's kept branch lives: its resolved
// worktree, else the seat's polecat directory.
func (d *Daemon) overrunWorktreePath(seat overrunSeat) string {
	polecatsDir := filepath.Join(d.config.TownRoot, seat.Rig, "polecats")
	if dir := resolvePolecatWorktree(polecatsDir, seat.Polecat, seat.Rig); dir != "" {
		return dir
	}
	return filepath.Join(polecatsDir, seat.Polecat)
}

// overrunResetAgentBead resets the stopped seat's agent bead the way gt polecat
// nuke does (hook_bead cleared, agent_state nuked), so the pool roster stops
// counting the seat as dead-hooked. A bead that is missing or no longer names
// seat.Bead is left alone: it was reset already, or the seat took new work.
func (d *Daemon) overrunResetAgentBead(seat overrunSeat) error {
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(d.config.TownRoot, seat.Rig), seat.Rig, seat.Polecat)
	return resetOverrunAgentBead(beads.New(d.config.TownRoot), id, seat.Bead)
}

// resetOverrunAgentBead is overrunResetAgentBead over c: it resets agent bead
// id only while its hook_bead is still bead.
func resetOverrunAgentBead(c beads.Client, id, bead string) error {
	_, err := beads.ResetAgentBeadWhileHooked(c, id, bead, "overrun stop")
	return err
}

// overrunRecordCleanupStatus records what a live probe of the released seat's
// worktree finds, so a later `gt polecat nuke` does not refuse the seat it was
// told to release for a cleanup_status=<missing> the release itself created
// (gt-6ixtp). The push runs before the hand-back, so a seat with a branch has
// it on origin by then; a seat on a detached HEAD has no branch to push, and
// the probe's unpushed count is what records its commits as at risk. A seat
// with no worktree has nothing to record.
func (d *Daemon) overrunRecordCleanupStatus(seat overrunSeat) error {
	polecatsDir := filepath.Join(d.config.TownRoot, seat.Rig, "polecats")
	workDir := resolvePolecatWorktree(polecatsDir, seat.Polecat, seat.Rig)
	if workDir == "" {
		d.logger.Printf("overrun_guard: %s/%s: no worktree to read a cleanup status from", seat.Rig, seat.Polecat)
		return nil
	}
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(d.config.TownRoot, seat.Rig), seat.Rig, seat.Polecat)
	return recordOverrunCleanupStatus(beads.New(d.config.TownRoot), id, overrunCleanupStatus(d.gitAt(workDir)))
}

// overrunCleanupStatus is the status a live probe of g's worktree supports:
// what the nuke preflight reads to decide whether removing the seat would lose
// work. A probe that cannot answer is unknown — the value a nuke reads the same
// way it reads a missing one — never clean.
func overrunCleanupStatus(g daemonGit) string {
	work, err := g.CheckUncommittedWork()
	if err != nil {
		return string(polecat.CleanupUnknown)
	}
	switch {
	case work.HasUncommittedChanges && !work.CleanExcludingRuntime():
		return string(polecat.CleanupUncommitted)
	case work.StashCount > 0:
		return string(polecat.CleanupStash)
	case work.UnpushedCommits > 0:
		// The push step names a branch, so a detached HEAD (or any push that
		// gave up) leaves its commits on this worktree alone: failing closed
		// here is what keeps a nuke from removing them.
		return string(polecat.CleanupUnpushed)
	}
	return string(polecat.CleanupClean)
}

// recordOverrunCleanupStatus is overrunRecordCleanupStatus over c: it writes
// status onto agent bead id.
func recordOverrunCleanupStatus(c beads.Client, id, status string) error {
	if id == "" {
		return errors.New("no agent bead id to record a cleanup status on")
	}
	return beads.UpdateAgentCleanupStatus(c, id, status)
}

// overrunStopLapsed reports why a stop that began on an earlier tick no longer
// owns its seat: the pause marker is gone (an operator resumed the seat) or a
// session is running on it. Either way the bead may belong to that session,
// and handing it back would release work in progress.
//
// Before the session is stopped (sessionStopped false) a running session is the
// one being stopped, so only the pause marker is read.
func (d *Daemon) overrunStopLapsed(seat overrunSeat, sessionStopped bool) (string, error) {
	paused, _, err := agentpause.IsPaused(d.config.TownRoot, seat.Rig, constants.RolePolecat, seat.Polecat)
	if err != nil {
		return "", fmt.Errorf("read pause marker: %w", err)
	}
	if !paused {
		return "the pause marker is gone", nil
	}
	if !sessionStopped {
		return "", nil
	}
	sessionName := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(seat.Rig), seat.Polecat)
	alive, err := d.tmux.HasSession(sessionName)
	if err != nil {
		return "", fmt.Errorf("check session %s: %w", sessionName, err)
	}
	if alive {
		return "a session is running on the seat", nil
	}
	return "", nil
}

// overrunSubmittedNow reads the seat's submitted state fresh: a live exiting
// heartbeat, or a work bead that is gt:ready-to-land. A bead read that fails
// answers no, as workBeadStillSubmitted does: the hand-back's own bd write
// fails when bd is down.
func (d *Daemon) overrunSubmittedNow(seat overrunSeat) bool {
	sessionName := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(seat.Rig), seat.Polecat)
	if d.gtDoneInFlight(sessionName) {
		return true
	}
	still, known := d.workBeadStillSubmitted(seat.Rig, seat.Bead)
	return known && still
}

// overrunStopSession stops the seat's session through the supervisor, the
// town's one kill path (gt-4k3fj.4): a town or rig e-stop refuses the stop,
// as it refuses every other automatic kill, with the supervisor's refusal
// text, and the seat's record gets the action line every stop writes. The
// park this stop wrote itself (step 1) does not refuse it — that park is
// what keeps scanners off the seat while the stop runs. A session that is
// already gone is a stop that has happened: killing a missing session
// succeeds.
func (d *Daemon) overrunStopSession(seat overrunSeat) error {
	supSeat := supervisor.SeatIn(d.prefixRegistry(), seat.Rig, constants.RolePolecat, seat.Polecat)
	return d.sup().KillOwnPark(supSeat, overrunStopReason(seat), overrunActor)
}

// errSandboxRecordCorrupt is a sandbox record that does not parse. The stop
// keeps the sandbox and hands the bead back with a note (overrunStop, step 2b);
// an unreadable file (an I/O error) is not this, and holds the stop as a refused
// push does, since a retry may read it.
var errSandboxRecordCorrupt = errors.New("sandbox record is corrupt")

// overrunRemoveSandbox removes the sandbox a stopped seat ran in, pushing its
// commits first (polecat.RemoveStoppedSandbox). pusher and deleter nil are the
// linked openshell hooks. No record found means nothing to remove and the stop
// proceeds: a host seat has no record. A record that cannot be read holds the
// stop, as a refused push does, and so does a lookup that cannot tell which
// record is the seat's. A refused step names the sandbox in its error, which the
// stop logs.
//
// The seat's record is read once and passed down, so the push and the delete
// act on the record the stop classified rather than one read again after it.
// The lookup of an unlisted rig reads the record to find it (overrunSandbox);
// a listed rig's record is read here.
func (d *Daemon) overrunRemoveSandbox(seat overrunSeat, pusher, deleter func(townRoot string, rec session.SandboxRecord) error) error {
	sb, err := d.overrunSandboxSession(seat)
	if err != nil || sb.Session == "" {
		return err
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
	if err := polecat.RemoveStoppedSandboxWithRecord(d.config.TownRoot, sb.Session, sb.Record, pusher, deleter); err != nil {
		return fmt.Errorf("sandbox %s: %w", sb.Session, err)
	}
	return nil
}

// overrunSandbox is the sandbox of a seat as the stop's lookup finds it.
type overrunSandbox struct {
	// Session names the sandbox record; "" means the seat has no sandbox to
	// remove.
	Session string
	// Record is the record the lookup read. It is set only when Read is: the
	// scan of a rig the registry does not list reads records to find the
	// seat's own, so it returns that record. A listed rig's record is read by
	// the caller.
	Record session.SandboxRecord
	// Read reports that Record holds the record the lookup read.
	Read bool
}

// readSandboxRecordFor is the sandbox record read of the stop: every record the
// sandbox step reads, the seat's or another seat's, goes through it. It is
// readSandboxRecord's when a test gave it one, else session.ReadSandboxRecord.
func (d *Daemon) readSandboxRecordFor(townRoot, sessionName string) (session.SandboxRecord, bool, error) {
	if d.readSandboxRecord != nil {
		return d.readSandboxRecord(townRoot, sessionName)
	}
	return session.ReadSandboxRecord(townRoot, sessionName)
}

// overrunSandboxSession finds the sandbox of the seat, or a Session of "" when
// the seat has none to remove. A rig the prefix registry lists has its session
// name built from the prefix, and the record left for the caller to read. A rig
// it does not list has no prefix to build one from, so its record is found by
// the rig and polecat it names.
func (d *Daemon) overrunSandboxSession(seat overrunSeat) (overrunSandbox, error) {
	reg := d.prefixRegistry()
	var known map[string]string
	if reg != nil {
		known = reg.AllRigs()
	}
	if _, ok := known[seat.Rig]; !ok {
		return d.overrunScanSandboxSession(seat)
	}
	name := session.PolecatSessionName(reg.PrefixForRig(seat.Rig), seat.Polecat)
	if !session.ValidSandboxSessionName(name) {
		d.logger.Printf("overrun_guard: WARNING: sandbox of %s: session name %q is not a sandbox record name, so the sandbox step is skipped", seat.Polecat, name)
		return overrunSandbox{}, nil
	}
	return overrunSandbox{Session: name}, nil
}

// overrunScanSandboxSession finds the sandbox record of a seat on a rig the
// registry does not list. One record naming the seat's rig and polecat is its
// sandbox, and the record the scan read is returned; none is a host seat, logged
// as a warning. Several records name it, or a record that may be the seat's own
// (its name ends in the polecat's) cannot be read, so the lookup cannot tell and
// holds: the seat's commits may be unpushed. A record that cannot be read and is
// named for another polecat is skipped with a warning.
func (d *Daemon) overrunScanSandboxSession(seat overrunSeat) (overrunSandbox, error) {
	entries, err := os.ReadDir(session.SandboxRegistryDir(d.config.TownRoot))
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return overrunSandbox{}, fmt.Errorf("sandbox of %s/%s: cannot list the sandbox records: %w", seat.Rig, seat.Polecat, err)
	}
	var found []string
	var foundRec session.SandboxRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		rec, ok, err := d.readSandboxRecordFor(d.config.TownRoot, name)
		if err != nil {
			if !overrunMayBeSeatsRecord(name, seat) {
				// Another seat's record that does not read cannot hold this
				// seat's stop: its name is not this polecat's, so it is not
				// this seat's sandbox. Holding on it would block every seat of
				// an unlisted rig for as long as the file stays broken.
				d.logger.Printf("overrun_guard: WARNING: sandbox of %s/%s: record %s cannot be read (%v) and its name is not the seat's, so it is skipped", seat.Rig, seat.Polecat, name, err)
				continue
			}
			return overrunSandbox{}, overrunRecordReadError(name, err)
		}
		if ok && rec.Rig == seat.Rig && rec.Polecat == seat.Polecat {
			found = append(found, name)
			foundRec = rec
		}
	}
	switch len(found) {
	case 0:
		d.logger.Printf("overrun_guard: WARNING: sandbox of %s: rig %s has no beads prefix in the registry and no sandbox record names it; nothing removed (set the prefix in mayor/rigs.json if this seat ran in a sandbox)", seat.Polecat, seat.Rig)
		return overrunSandbox{}, nil
	case 1:
		return overrunSandbox{Session: found[0], Record: foundRec, Read: true}, nil
	default:
		return overrunSandbox{}, fmt.Errorf("sandbox of %s/%s: %d records name it (%s), cannot tell which to remove", seat.Rig, seat.Polecat, len(found), strings.Join(found, ", "))
	}
}

// overrunMayBeSeatsRecord reports whether a sandbox record named name could be
// seat's: a session name is a rig prefix, a dash and the polecat's name. A
// record whose name ends otherwise is another seat's.
func overrunMayBeSeatsRecord(name string, seat overrunSeat) bool {
	return name == seat.Polecat || strings.HasSuffix(name, "-"+seat.Polecat)
}

// overrunRecordReadError is the error for a sandbox record that cannot be read:
// a record that does not parse is errSandboxRecordCorrupt, which the stop hands
// back with a note and keeps the seat paused; any other failure (an I/O error)
// is returned as it is, which holds the stop, since a retry may read it.
func overrunRecordReadError(name string, err error) error {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typ) {
		return fmt.Errorf("sandbox %s: %w: %w", name, errSandboxRecordCorrupt, err)
	}
	return fmt.Errorf("sandbox %s: %w", name, err)
}

// overrunStopReason is what the supervisor's action log records for the kill:
// the rule, and the bead it stops the seat over.
func overrunStopReason(seat overrunSeat) string {
	return fmt.Sprintf("%s on %s", overrunPauseReason, seat.Bead)
}

// collectOverrunStopped raises one low item per bead the guard handed back and
// an operator has not yet split: the bead still carries needs-split. The item
// names the bead and how long the polecat ran, read from the guard's record;
// removing the label clears it.
func (s *attentionSources) collectOverrunStopped(ctx context.Context) ([]attention.Item, error) {
	var out []attention.Item
	for _, rigName := range s.d.getKnownRigs() {
		issues, err := s.d.rigWorkBeads(rigName).List(beads.ListOptions{
			Status: "open", Label: polecat.NeedsSplitLabel, Priority: -1, Limit: 0,
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rigName, err)
		}
		for _, is := range issues {
			if is == nil {
				continue
			}
			summary := is.ID + " needs a split"
			if st, err := readOverrunState(s.d.config.TownRoot, is.ID); err == nil && !st.StoppedAt.IsZero() {
				summary = fmt.Sprintf("%s stopped after %s (20 minute rule); needs a split", is.ID, st.Elapsed.Round(time.Second))
			}
			out = append(out, attention.Item{
				Key:      "overrun:" + is.ID,
				Kind:     attention.KindOverrunStopped,
				Severity: attention.SeverityLow,
				Rig:      rigName,
				Bead:     is.ID,
				Summary:  summary,
			})
		}
	}
	return out, nil
}

// overrunParkedSeatAfter is how long a seat the guard left paused may sit
// before the queue asks an operator to look. A stop that finishes releases the
// seat in its own last step, so a pause that outlives an hour is a stop that
// has not reached it — it gave up on the checkpoint or push, met a sandbox
// record that does not parse, or is still retrying one of those — and the seat
// holds its pool seat and worktree with nothing but a once-a-tick log line
// saying so (gt-ftva5).
const overrunParkedSeatAfter = time.Hour

// collectOverrunParkedSeats raises one low item per polecat seat whose intent
// record is still the overrun guard's park after overrunParkedSeatAfter: a
// stop that never released its seat. The item names the seat, the bead the
// seat holds, how long it has been parked and the two commands an operator
// runs — check-recovery first, then nuke once git shows the work is safe. It
// clears when the pause ends (the guard's own release, or gt agent resume) or
// the seat is nuked, because the walk then finds no such park.
//
// The record is the only input: paused by overrunActor and not frozen is the
// park the guard writes, and a seat an operator parked or the supervisor froze
// is not this item's. An unreadable record fails the collection rather than
// passing as clean, so the previous tick's items are kept (collectAttention).
// The key is the seat, so a seat raises one item however many ticks it stays
// parked.
func (s *attentionSources) collectOverrunParkedSeats(_ context.Context) ([]attention.Item, error) {
	dir := filepath.Join(constants.TownRuntimePath(s.d.config.TownRoot), "agents")
	var out []attention.Item
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && path == dir {
				return filepath.SkipDir
			}
			return err
		}
		if e.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		// A path WalkDir handed back from under dir is always relative to it
		// (both are absolute and on one volume), so Rel has no error to take.
		rel, _ := filepath.Rel(dir, path)
		rig := filepath.Dir(rel)
		if rig == "." {
			return nil // a town-level seat, never a polecat's
		}
		stem := strings.TrimSuffix(filepath.Base(rel), ".json")
		role, name, named := strings.Cut(stem, ".")
		if !named || role != constants.RolePolecat || name == "" {
			return nil
		}
		rec, rerr := intent.ReadPath(path)
		if rerr != nil {
			return fmt.Errorf("%s: %w", rel, rerr)
		}
		if !overrunGuardParked(rec) {
			return nil
		}
		parked := s.now.Sub(rec.PausedAt)
		if parked <= overrunParkedSeatAfter {
			return nil
		}
		seat := rig + "/" + name
		bead := rec.WorkBead
		if bead == "" {
			bead = "no recorded bead"
		}
		out = append(out, attention.Item{
			Key:      "overrun-parked:" + seat,
			Kind:     attention.KindOverrunStopped,
			Severity: attention.SeverityLow,
			Rig:      rig,
			Bead:     rec.WorkBead,
			Summary: fmt.Sprintf("%s paused by the overrun guard %s ago on %s; recover with gt polecat check-recovery %s, then gt polecat nuke %s after a live git check",
				seat, parkedAge(parked), bead, seat, seat),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// overrunGuardParked reports whether rec is the park the overrun guard writes,
// read with the shape of the supervisor's own-park check: its actor,
// desired=park, and no supervisor freeze. An unset paused_at is not a park
// this collector can age.
func overrunGuardParked(rec intent.Record) bool {
	return !rec.Frozen && rec.Desired == intent.DesiredPark && rec.PausedBy == overrunActor && !rec.PausedAt.IsZero()
}
