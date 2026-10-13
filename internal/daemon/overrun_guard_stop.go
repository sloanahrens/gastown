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
//     roster no longer counts the seat. The attention item for the stop is
//     derived from that hand-back (collectOverrunStopped).
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
	branch := st.Branch
	if branch == "" {
		branch = "(unknown)"
	}
	elapsed = st.Elapsed

	// 1. Pause.
	if st.StopBeganAt.IsZero() {
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

	// 4. Hand the bead back.
	handed := seat
	handed.Bead, handed.Branch = st.Bead, branch
	if err := s.handBack(handed, elapsed); err != nil {
		d.logger.Printf("overrun_guard: stop %s on %s: hand-back failed: %v", who, st.Bead, err)
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
	s.checkpoint = func(seat overrunSeat) error {
		polecatsDir := filepath.Join(d.config.TownRoot, seat.Rig, "polecats")
		workDir := resolvePolecatWorktree(polecatsDir, seat.Polecat, seat.Rig)
		if workDir == "" {
			// Nothing to checkpoint; the branch is whatever it already holds.
			d.logger.Printf("overrun_guard: %s/%s: no worktree to checkpoint", seat.Rig, seat.Polecat)
			return nil
		}
		d.checkpointWorktree(workDir, seat.Rig, seat.Polecat)
		return nil
	}
	s.handBack = func(seat overrunSeat, elapsed time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), overrunHandBackTimeout)
		defer cancel()
		store := beads.NewPlain(d.config.TownRoot, bdMutationRoutingEnv(d.config.TownRoot),
			beads.WithBin(d.bdPathOrDefault())).WithTimeout(overrunHandBackTimeout)
		return polecat.HandBack(ctx, polecat.NewBeadWriter(store, overrunActor), seat.Bead, seat.Branch, elapsed)
	}
	return s
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
