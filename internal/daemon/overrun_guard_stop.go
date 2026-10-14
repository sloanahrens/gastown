package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
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
//     else, the gt-fojqs ordering: a live seat must not stay parked);
//  3. checkpoint the worktree;
//  4. hand the bead back (status open, assignee cleared, needs-split) so the
//     roster no longer counts the seat; a bead found closed or re-slung to
//     another polecat is left alone. The attention item for the stop is
//     derived from that hand-back (collectOverrunStopped);
//  5. release the seat: clear the pause, so the polecat name and its worktree
//     can take new work. The kept branch is safe without the park: the
//     bead's comment names it and the worktree is not touched.
//
// submittedNow is read before step 1 and before step 4: a seat that has
// submitted (gt done running, or gt:ready-to-land) is not stopped, and a bead
// that gained gt:ready-to-land mid-stop is not handed back.
//
// A refused checkpoint fails step 3. A stop resumed past step 2 first checks
// the pause is still down and no session runs on the seat; if not, the seat
// was taken back and the stop ends without a hand-back.
//
// Progress is saved after each step, so a step that fails is logged and
// retried from the failed step on the next tick, never from the top and never
// skipping ahead. StoppedAt is written last; with it set the stop does not run
// again for this sling. It never deletes a branch or a worktree and never
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
	}
	resumed := !st.SessionStoppedAt.IsZero()
	branch := st.Branch
	if branch == "" {
		branch = "(unknown)"
	}
	elapsed = st.Elapsed

	// 1. Pause. The seat list is older than this call, so look again first: a
	// seat that has begun gt done since is not stopped.
	if st.StopBeganAt.IsZero() {
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

	// 2. Stop the session.
	if st.SessionStoppedAt.IsZero() {
		if err := s.stopSession(seat); err != nil {
			d.logger.Printf("overrun_guard: stop %s on %s: session stop failed: %v", who, st.Bead, err)
			if cerr := s.clearPause(seat); cerr != nil {
				// Keep the stop pending: the next tick tries the stop again.
				d.logger.Printf("overrun_guard: WARNING: %s: pause marker not cleared: %v", who, cerr)
				return
			}
			st.StopBeganAt = time.Time{}
			save()
			return
		}
		st.SessionStoppedAt = now
		if !save() {
			return
		}
	}

	// A stop resumed after its session was stopped may find the seat taken
	// back: an operator lifted the pause, or a new session started on it.
	// Steps 3 and 4 then act on a bead that session holds, so the stop ends.
	if resumed && s.stopLapsed != nil {
		why, err := s.stopLapsed(seat)
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

	// 3. Checkpoint the worktree.
	if st.CheckpointedAt.IsZero() {
		if err := s.checkpoint(seat); err != nil {
			d.logger.Printf("overrun_guard: stop %s on %s: checkpoint failed: %v", who, st.Bead, err)
			return
		}
		st.CheckpointedAt = now
		if !save() {
			return
		}
	}

	// 4. Hand the bead back, unless the work was submitted while the stop ran:
	// a bead that is gt:ready-to-land must not also carry needs-split. The seat
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
	handed := seat
	handed.Bead, handed.Branch = st.Bead, branch
	if err := s.handBack(handed, elapsed); err != nil {
		d.logger.Printf("overrun_guard: stop %s on %s: hand-back failed: %v", who, st.Bead, err)
		return
	}
	// 5. Release the seat. The bead no longer names it, so nothing restarts it
	// on this sling; leaving the pause would make parkedReuseBlocker refuse the
	// name until an operator ran gt agent resume, with no notice that it was
	// parked. A failed clear leaves the stop pending, so the next tick repeats
	// the hand-back (HandBack finishes a bead it already released) and the clear.
	if err := s.clearPause(seat); err != nil {
		d.logger.Printf("overrun_guard: WARNING: %s: pause marker not cleared after the hand-back of %s: %v", who, st.Bead, err)
		return
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
		seat := overrunSeat{Rig: st.Rig, Polecat: st.Polecat, Bead: st.Bead, Branch: st.Branch}
		d.overrunStop(s, seat, st, st.Elapsed)
	}
	return touched
}

// overrunStopSeams fills the stop's side effects with the production ones.
func (d *Daemon) overrunStopSeams(s overrunSeams) overrunSeams {
	s.pause = func(seat overrunSeat) error {
		return agentpause.Pause(d.config.TownRoot, seat.Rig, constants.RolePolecat, seat.Polecat,
			overrunPauseReason, overrunActor, "")
	}
	s.clearPause = func(seat overrunSeat) error {
		return agentpause.Resume(d.config.TownRoot, seat.Rig, constants.RolePolecat, seat.Polecat)
	}
	s.stopSession = d.overrunStopSession
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
	s.handBack = func(seat overrunSeat, elapsed time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), overrunHandBackTimeout)
		defer cancel()
		store := beads.NewPlain(d.config.TownRoot, bdMutationRoutingEnv(d.config.TownRoot),
			beads.WithBin(d.bdPathOrDefault())).WithTimeout(overrunHandBackTimeout)
		err := polecat.HandBack(ctx, polecat.NewBeadWriter(store, overrunActor), seat.Bead,
			seat.Rig+"/polecats/"+seat.Polecat, seat.Branch, elapsed)
		if errors.Is(err, polecat.ErrHandBackSkipped) {
			// The bead is no longer this seat's: nothing to release, nothing to repeat.
			d.logger.Printf("overrun_guard: %s/%s: %v", seat.Rig, seat.Polecat, err)
			return nil
		}
		return err
	}
	return s
}

// overrunStopLapsed reports why a stop that began on an earlier tick no longer
// owns its seat: the pause marker is gone (an operator resumed the seat) or a
// session is running on it. Either way the bead may belong to that session,
// and handing it back would release work in progress.
func (d *Daemon) overrunStopLapsed(seat overrunSeat) (string, error) {
	paused, _, err := agentpause.IsPaused(d.config.TownRoot, seat.Rig, constants.RolePolecat, seat.Polecat)
	if err != nil {
		return "", fmt.Errorf("read pause marker: %w", err)
	}
	if !paused {
		return "the pause marker is gone", nil
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

// overrunStopSession stops the seat's session through the polecat session
// manager. A session that is already gone is a stop that has happened.
func (d *Daemon) overrunStopSession(seat overrunSeat) error {
	t, ok := d.tmux.(*tmux.Tmux)
	if !ok {
		return errors.New("daemon tmux is not the real tmux; cannot build a polecat session manager")
	}
	r := &rig.Rig{Name: seat.Rig, Path: filepath.Join(d.config.TownRoot, seat.Rig)}
	err := polecat.NewSessionManager(t, r, d.prefixRegistry()).Stop(seat.Polecat, false)
	if errors.Is(err, polecat.ErrSessionNotFound) {
		return nil
	}
	return err
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
