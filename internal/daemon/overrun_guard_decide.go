package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// overrunAction is what the overrun guard does for one seat on one tick.
type overrunAction int

const (
	overrunNone overrunAction = iota
	overrunNudge
	overrunStop
)

// overrunInput is everything decideOverrun reads. The caller supplies Now so
// the decision never touches a clock.
type overrunInput struct {
	Now, SlungAt    time.Time
	Nudged          bool          // the nudge already went out for this sling
	NudgeAt, StopAt time.Duration // elapsed since SlungAt
	// GateSpent is how much of this sling the seat spent inside its own gate
	// (overrun_guard_gate.go): the 12 and 20 minute rule measures the rest of
	// the time, never the gate.
	GateSpent time.Duration
}

// decideOverrun returns the action for one seat. A zero SlungAt or one after
// Now means the sling time is unknown or wrong, so it acts on nothing. Stop
// wins over nudge, and fires even when the seat was never nudged. A nudge fires
// once: Nudged suppresses it on later ticks.
//
// The elapsed time it judges is the sling's wall time less GateSpent, so a seat
// waiting on its own gate runs past both thresholds without a nudge or a stop,
// and one whose non-gate time passes the stop threshold is stopped even while
// it is in a gate (gt-vfwuc). A GateSpent past the wall time leaves a negative
// elapsed, which is under both thresholds: acting on nothing.
func decideOverrun(in overrunInput) overrunAction {
	if in.SlungAt.IsZero() || in.SlungAt.After(in.Now) {
		return overrunNone
	}
	elapsed := in.Now.Sub(in.SlungAt) - in.GateSpent
	switch {
	case elapsed >= in.StopAt:
		return overrunStop
	case elapsed >= in.NudgeAt && !in.Nudged:
		return overrunNudge
	}
	return overrunNone
}

const (
	// overrunNudgeRetryAfter is the least wait after a failed nudge before the
	// next attempt for the same sling.
	overrunNudgeRetryAfter = 5 * time.Minute
	// overrunNudgeMaxAttempts is how many nudges the guard tries per sling
	// before it gives up on the nudge. The stop never depends on it.
	overrunNudgeMaxAttempts = 3
	// overrunCheckpointMaxAttempts is how many failed checkpoints a stop
	// tolerates, one per tick, before it hands the bead back without one. A
	// refusal such as a mid-rebase worktree does not clear while the seat is
	// stopped, so retrying past a few ticks only keeps the bead on a dead seat.
	overrunCheckpointMaxAttempts = 5
	// overrunPushMaxAttempts is how many failed pushes of the kept branch a stop
	// tolerates, one per tick, before it hands the bead back with a note that the
	// branch exists only in the worktree. A remote that refuses the branch for
	// good would otherwise hold the bead on a paused seat with a log line per tick.
	overrunPushMaxAttempts = 5
	// overrunCommentMaxAttempts is how many failed bead comments a scheduled
	// stop tolerates, one per tick, before it ends without one.
	overrunCommentMaxAttempts = 5
)

// nudgeFailures is how many nudges have failed for the sling at slung.
func (s overrunState) nudgeFailures(slung time.Time) int {
	if s.NudgeFailedAt.Before(slung) {
		return 0
	}
	return s.NudgeFailures
}

// nudgeHeld reports whether a nudge due at now must wait: the attempts for
// this sling are used up, or the last one failed less than
// overrunNudgeRetryAfter ago.
func (s overrunState) nudgeHeld(slung, now time.Time) bool {
	n := s.nudgeFailures(slung)
	return n >= overrunNudgeMaxAttempts || (n > 0 && now.Sub(s.NudgeFailedAt) < overrunNudgeRetryAfter)
}

// withGate folds one tick's gate reading into the record and returns it with
// the gate time to measure this tick's elapsed against. It charges the interval
// since the last tick that saw the seat inside its gate, never past maxTotal for
// the sling, and it drops the accounting of an earlier sling — a rework starts
// a new clock, and the gate time of the run before it is not this run's.
//
// A stretch is charged only across intervals whose both ends were seen inside
// the gate, and no tick charges more than maxCharge (twice the guard's
// interval, see overrunMaxGateCharge): the guard cannot date the start of a
// gate, nor the end of one that ended between ticks, and a tick it skipped — a
// restart, a starved cycle — leaves a gap in which the seat may have left and
// re-entered. What it cannot watch it does not charge, which keeps the clock
// counting (the fail-safe direction, gt-vfwuc).
//
// maxTotal is the ceiling on the whole sling (overrunMaxGateTotal): a gate that
// does not return is a stuck seat, and past the ceiling the clock runs again
// even inside the gate.
func (s overrunState) withGate(slung, now time.Time, inGate bool, maxCharge, maxTotal time.Duration) (overrunState, time.Duration) {
	if !s.GateSlingAt.Equal(slung) {
		s.GateSlingAt, s.GateSpent, s.GateSeenAt = slung, 0, time.Time{}
	}
	switch {
	case inGate && s.GateSeenAt.IsZero():
		// The first tick that found the gate: nothing before it is charged.
		s.GateSeenAt = now
	case inGate && now.After(s.GateSeenAt):
		s.GateSpent += min(now.Sub(s.GateSeenAt), maxCharge)
		s.GateSeenAt = now
	case inGate:
		// A stamp at or after now is a clock that stepped back. Drop the
		// stretch rather than charge a negative interval, which would give back
		// gate time this sling was already credited with; the next tick starts
		// it again, and what is not charged counts toward the stop (gt-vfwuc).
		s.GateSeenAt = time.Time{}
	default:
		// The gate is over: what it ran past the last tick that saw it open is
		// not charged, because the guard cannot tell that from work the seat
		// did after it.
		s.GateSeenAt = time.Time{}
	}
	if s.GateSpent > maxTotal {
		// The ceiling binds whatever the record held, a state file written
		// before it existed included.
		s.GateSpent = maxTotal
	}
	return s, s.GateSpent
}

// sameGate reports whether two records carry the same gate accounting, so a
// tick writes the state only when one moved.
func (s overrunState) sameGate(o overrunState) bool {
	return s.GateSeenAt.Equal(o.GateSeenAt) && s.GateSpent == o.GateSpent && s.GateSlingAt.Equal(o.GateSlingAt)
}

// overrunState is what the guard remembers about one bead between ticks.
//
// The stop's progress is kept here too, one stamp per step, because every
// step after the pause outlives the seat's visibility: once the session is
// gone the seat is no longer a working seat, so a step that fails is retried
// from this record (pendingOverrunStops), not from the seat list.
type overrunState struct {
	Bead     string    `json:"bead"`
	NudgedAt time.Time `json:"nudged_at,omitzero"`

	// NudgeFailures counts the failed nudges for the sling that NudgeFailedAt
	// belongs to; NudgeFailedAt is when the latest one failed. A failure
	// stamped before the current sling belongs to an earlier run of the bead
	// and counts for nothing (nudgeFailures).
	NudgeFailures int       `json:"nudge_failures,omitempty"`
	NudgeFailedAt time.Time `json:"nudge_failed_at,omitzero"`

	// GateSlingAt is the sling the gate accounting below belongs to, GateSpent
	// the gate time charged for it (at most overrunMaxGateTotal for the sling),
	// and GateSeenAt — when set — the last tick that saw the seat inside its
	// gate (see withGate). The guard measures the sling's elapsed time as
	// (now - slung) - GateSpent, so the time a seat waits inside its own gate
	// does not count toward the 12 and 20 minute rule, and a gate that never
	// returns does (gt-vfwuc).
	GateSlingAt time.Time     `json:"gate_sling_at,omitzero"`
	GateSpent   time.Duration `json:"gate_spent,omitempty"`
	GateSeenAt  time.Time     `json:"gate_seen_at,omitzero"`

	// Rig, Polecat and Branch name the seat the stop acts on; Elapsed is how
	// long it had held the bead when the stop began, gate time excluded.
	Rig     string        `json:"rig,omitempty"`
	Polecat string        `json:"polecat,omitempty"`
	Branch  string        `json:"branch,omitempty"`
	Elapsed time.Duration `json:"elapsed,omitempty"`
	// SlingAt dates the sling the stop began on, so a stop resumed on a later
	// tick still tells a head the seat produced from the branch it resumed
	// (overrunFinishedOnOrigin).
	SlingAt time.Time `json:"sling_at,omitzero"`

	// StopBeganAt is when the pause marker went down, SessionStoppedAt when
	// the session was confirmed gone, CheckpointedAt when the worktree
	// checkpoint step finished. StoppedAt is set last, once the bead is handed
	// back; with it set the stop does not run again for this sling.
	StopBeganAt      time.Time `json:"stop_began_at,omitzero"`
	SessionStoppedAt time.Time `json:"session_stopped_at,omitzero"`
	CheckpointedAt   time.Time `json:"checkpointed_at,omitzero"`
	// PushedAt is when the kept branch reached origin; the hand-back waits for it.
	PushedAt time.Time `json:"pushed_at,omitzero"`

	// CheckpointFailures counts the checkpoint attempts that failed in this
	// stop. At overrunCheckpointMaxAttempts the stop hands the bead back
	// without a checkpoint and sets CheckpointGaveUp, so a worktree that
	// refuses every attempt does not hold the bead for good.
	CheckpointFailures int  `json:"checkpoint_failures,omitempty"`
	CheckpointGaveUp   bool `json:"checkpoint_gave_up,omitempty"`

	// PushFailures counts the failed pushes of the kept branch in this stop.
	// At overrunPushMaxAttempts, or at once for a non-fast-forward rejection,
	// the stop hands the bead back with a note and sets PushGaveUp;
	// PushFailure is why the last push failed, kept for the note.
	PushFailures int    `json:"push_failures,omitempty"`
	PushGaveUp   bool   `json:"push_gave_up,omitempty"`
	PushFailure  string `json:"push_failure,omitempty"`
	// ReworkSHA is the commit the kept branch held on origin before the stop's
	// own push, when it was the worktree's head too: the finished work was
	// already there, so the hand-back is rework (overrunStop, step 5).
	// ReworkDecidedAt is when that was read (before the push); the answer is
	// saved with it, so a save that fails after the push cannot lose it, and a
	// retry keeps the decision instead of reading the pushed origin.
	ReworkSHA       string    `json:"rework_sha,omitempty"`
	ReworkDecidedAt time.Time `json:"rework_decided_at,omitzero"`

	StoppedAt time.Time `json:"stopped_at,omitzero"`

	// Scheduled marks a stop of a scheduled sling (overrunScheduledStop): it
	// stops the session, comments once, closes the run bead and releases the
	// seat, so a pending one is resumed there and never by overrunStop, which
	// would hand the bead back as work. CommentFailures counts the failed
	// comments; at overrunCommentMaxAttempts the stop goes on without one.
	// CommentedAt and ClosedAt stamp those two steps so a retry of a later
	// step does not repeat them; CloseFailures counts the failed closes, and
	// at overrunCommentMaxAttempts the stop ends with the seat left paused and
	// the bead hooked, as before this release step existed.
	Scheduled       bool      `json:"scheduled,omitempty"`
	CommentFailures int       `json:"comment_failures,omitempty"`
	CommentedAt     time.Time `json:"commented_at,omitzero"`
	ClosedAt        time.Time `json:"closed_at,omitzero"`
	CloseFailures   int       `json:"close_failures,omitempty"`
}

// overrunStatePath is <town>/.runtime/overrun-guard/<bead>.json. A bead id
// that could leave that directory is an error.
func overrunStatePath(townRoot, bead string) (string, error) {
	if bead == "" || bead != filepath.Base(bead) || strings.ContainsAny(bead, `/\`) {
		return "", fmt.Errorf("overrun guard: invalid bead id %q", bead)
	}
	return filepath.Join(townRoot, ".runtime", "overrun-guard", bead+".json"), nil
}

// readOverrunState loads the state for bead. A missing file is the zero state
// (nothing has happened yet), not an error.
func readOverrunState(townRoot, bead string) (overrunState, error) {
	path, err := overrunStatePath(townRoot, bead)
	if err != nil {
		return overrunState{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return overrunState{}, nil
	}
	if err != nil {
		return overrunState{}, fmt.Errorf("read overrun state: %w", err)
	}
	var s overrunState
	if err := json.Unmarshal(data, &s); err != nil {
		return overrunState{}, fmt.Errorf("parse overrun state %s: %w", path, err)
	}
	return s, nil
}

// writeOverrunState saves s atomically: it writes a temp file in the same
// directory and renames it over the target, so a reader never sees half a file.
func writeOverrunState(townRoot string, s overrunState) error {
	path, err := overrunStatePath(townRoot, s.Bead)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create overrun state dir: %w", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode overrun state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, s.Bead+".*.tmp")
	if err != nil {
		return fmt.Errorf("create overrun state temp file: %w", err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write overrun state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace overrun state: %w", err)
	}
	return nil
}

// pendingOverrunStops lists the beads whose stop began but did not finish: the
// pause marker is down and the bead is not yet handed back. A state file that
// cannot be read is skipped; its bead is judged again from the seat list.
func pendingOverrunStops(townRoot string) []overrunState {
	dir := filepath.Join(townRoot, ".runtime", "overrun-guard")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []overrunState
	for _, e := range entries {
		bead, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		st, err := readOverrunState(townRoot, bead)
		if err != nil || st.StopBeganAt.IsZero() || !st.StoppedAt.IsZero() {
			continue
		}
		st.Bead = bead
		out = append(out, st)
	}
	return out
}
