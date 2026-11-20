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
	Nudged          bool          // the seat was already marked for this sling
	NudgeAt, StopAt time.Duration // elapsed since SlungAt
	// GateSpent is how much of this sling the seat spent inside its own gate
	// (overrun_guard_gate.go): the 12 and 20 minute rule measures the rest of
	// the time, never the gate.
	GateSpent time.Duration
	// EscalationSpent is how much of this sling the seat spent waiting on an
	// open escalation it raised (overrun_guard_escalation.go), the same way.
	EscalationSpent time.Duration
}

// decideOverrun returns the action for one seat. A zero SlungAt or one after
// Now means the sling time is unknown or wrong, so it acts on nothing. Stop
// wins over the nudge threshold, and fires even when the seat was never marked.
// The threshold fires once: Nudged suppresses it on later ticks.
//
// The elapsed time it judges is the sling's wall time less GateSpent and
// EscalationSpent, so a seat waiting on its own gate or on an escalation it
// raised runs past both thresholds without a mark or a stop, and one whose
// remaining time passes the stop threshold is stopped even while it is in a
// gate (gt-vfwuc). Spent time past the wall time leaves a negative elapsed,
// which is under both thresholds: acting on nothing.
func decideOverrun(in overrunInput) overrunAction {
	if in.SlungAt.IsZero() || in.SlungAt.After(in.Now) {
		return overrunNone
	}
	elapsed := in.Now.Sub(in.SlungAt) - in.GateSpent - in.EscalationSpent
	switch {
	case elapsed >= in.StopAt:
		return overrunStop
	case elapsed >= in.NudgeAt && !in.Nudged:
		return overrunNudge
	}
	return overrunNone
}

const (
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
	// overrunSaveMaxAttempts is how many refused saves a stop tolerates, one per
	// tick, before it parks the seat with the refusal as the reason. A save is a
	// checkpoint and a push, so it takes the checkpoint's bound.
	overrunSaveMaxAttempts = overrunCheckpointMaxAttempts
	// overrunOriginReadMaxAttempts is the cap on the failed origin reads a stop
	// counts in overrunState.OriginReadFailures, one per tick: a read of origin
	// that fails, a sandbox record naming no branch, and a worktree that does
	// not resolve. The count stops at the cap. A stop short of it holds the bead
	// and retries on the next tick.
	overrunOriginReadMaxAttempts = 5
	// overrunOriginRaiseMaxAttempts is how many failed raises of the cap's
	// high-severity item a stop tolerates, one per tick, before it hands the
	// bead back without one. The raise comes before the hand-back so a bead is
	// never released without its item, but an escalation store that stays down
	// would otherwise hold the bead for good: the drop is recorded in the town
	// feed and the hand-back note carries the reason.
	overrunOriginRaiseMaxAttempts = 5
	// overrunCommentMaxAttempts is how many failed bead comments a scheduled
	// stop tolerates, one per tick, before it ends without one.
	overrunCommentMaxAttempts = 5
)

// overrunSessionGrace is how long after a sling the seat's session may have
// been created and still belong to that sling: the time the sling spent
// preparing the session (a sandbox's image resolve, create and wait). A session
// created later than this after the sling belongs to a later dispatch.
const overrunSessionGrace = 5 * time.Minute

// slungFloor is the dispatch time the guard measures a run from, never earlier
// than the session that is running it. The event log dates a bead's run from its
// earliest sling, so a sling whose session never started leaves a date that a
// later sling of the same bead inherits; the polecat that finally starts would
// be stopped for time it never ran (gt-i6recg). A session created more than
// overrunSessionGrace after the sling cannot be that sling's, so the run is
// measured from the session. A zero sessionStart (unknown) leaves the sling as
// it is. A restart of the seat after the sling already accounts for a later
// session (withRestart restarts the clock, within its cap), so it is left to that
// rule.
func slungFloor(slung, sessionStart, restarted time.Time) time.Time {
	if sessionStart.IsZero() || restarted.After(slung) {
		return slung
	}
	if sessionStart.After(slung.Add(overrunSessionGrace)) {
		return sessionStart
	}
	return slung
}

// overrunMaxRestartClocks is how many restarts of one run may each start its
// clock over. A session restarted hours after its seat died gets a fresh clock,
// but a seat that keeps dying and coming back would otherwise never be stopped:
// each restart would restart the count. Past this many, the clock stays where
// the last allowed restart left it, so the seat is judged on that clock. The
// supervisor freezes a seat that restarts three times in an hour; this cap
// bounds the slower loops that budget never sees (gt-y9lkx).
const overrunMaxRestartClocks = 3

// withRestart folds the seat's latest restart into the record and returns the
// time the clock measures from: the later of the sling and the restart adopted
// for it. A session restarted hours after its seat died starts its time over,
// because the dead stretch was not time a session was alive on the bead; a seat
// that never died measures from its sling as before (gt-y9lkx).
//
// An adopted restart is kept across ticks and written to the state file: the
// intent record's stamps last only the supervisor's budget window. A restart
// recorded for an earlier sling is dropped, the way the gate accounting is: a
// rework starts a new clock. A stamp at or before the sling predates this
// dispatch and is not a restart of it. At most overrunMaxRestartClocks restarts
// of one run are adopted.
func (s overrunState) withRestart(slung, restarted time.Time) (overrunState, time.Time) {
	if !s.RestartSlingAt.Equal(slung) {
		s.RestartSlingAt, s.RestartedAt, s.RestartResets = slung, time.Time{}, 0
	}
	if restarted.After(slung) && restarted.After(s.RestartedAt) && s.RestartResets < overrunMaxRestartClocks {
		s.RestartedAt = restarted
		s.RestartResets++
	}
	return s, s.restartClock(slung)
}

// restartClock is the time the clock measures from for a run dispatched at
// slung: the adopted restart when it belongs to that run and is later, else
// the sling.
func (s overrunState) restartClock(slung time.Time) time.Time {
	if s.RestartSlingAt.Equal(slung) && s.RestartedAt.After(slung) {
		return s.RestartedAt
	}
	return slung
}

// OverrunRestartClock is the clock the overrun guard judges a bead's run by,
// read from the guard's saved state rather than recomputed: the restart the
// guard adopted for the run dispatched at slung, else slung. The dashboard's Age
// reads it, so a restarted seat shows the age the guard stops it on. The guard
// adopts a restart on its next tick, so the Age lags a restart by one tick. An
// unreadable state answers slung; the guard reports the unreadable record itself.
func OverrunRestartClock(townRoot, bead string, slung time.Time) time.Time {
	s, err := readOverrunState(townRoot, bead)
	if err != nil {
		return slung
	}
	return s.restartClock(slung)
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
// maxTotal is the ceiling on the whole sling (the configured gate_allowance,
// overrunMaxGateTotal by default): a gate that does not return is a stuck
// seat, and past the ceiling the clock runs again even inside the gate.
func (s overrunState) withGate(slung, now time.Time, inGate bool, maxCharge, maxTotal time.Duration) (overrunState, time.Duration) {
	if !s.GateSlingAt.Equal(slung) {
		s.GateSlingAt, s.GateSpent, s.GateSeenAt = slung, 0, time.Time{}
	}
	s.GateSpent, s.GateSeenAt = chargeWait(s.GateSpent, s.GateSeenAt, now, inGate, maxCharge, maxTotal)
	return s, s.GateSpent
}

// overrunGateReadCarryMax is how many consecutive failed gate reads may still
// carry the last answer. Past it the guard counts all of the seat's time, the
// direction that stops: a gate read that stays broken is a guard that cannot
// tell a working seat from a stuck one.
const overrunGateReadCarryMax = 2

// recordGateRead folds one gate read into the record: a successful read keeps
// its answer and starts the run of failures over, a failed one lengthens it.
func (s *overrunState) recordGateRead(ok, inGate bool) {
	if !ok {
		s.GateReadFailures++
		return
	}
	s.GateIn, s.GateReadFailures = inGate, 0
}

// carriedGate is the answer of the last successful gate read, for the caller to
// use while reads fail: it carries for at most overrunGateReadCarryMax
// consecutive failures and answers false, false past that, so the caller counts
// all of the seat's time. Only an in-gate answer is carried — outside is the
// answer the caller falls back to anyway, and a false answer is the zero value
// a record with no answer holds, so it cannot be told from one never read.
func (s overrunState) carriedGate() (inGate, carried bool) {
	if s.GateIn && s.GateReadFailures <= overrunGateReadCarryMax {
		return true, true
	}
	return false, false
}

// withEscalation is withGate for the seat's own open escalation
// (overrun_guard_escalation.go): the same banking, kept in its own counters
// with its own ceiling, so a forgotten escalation cannot hold the clock for good.
func (s overrunState) withEscalation(slung, now time.Time, waiting bool, maxCharge, maxTotal time.Duration) (overrunState, time.Duration) {
	if !s.EscalationSlingAt.Equal(slung) {
		s.EscalationSlingAt, s.EscalationSpent, s.EscalationSeenAt = slung, 0, time.Time{}
	}
	s.EscalationSpent, s.EscalationSeenAt = chargeWait(s.EscalationSpent, s.EscalationSeenAt, now, waiting, maxCharge, maxTotal)
	return s, s.EscalationSpent
}

// chargeWait folds one tick's reading of one kind of wait into the time banked
// for it and the last tick that saw it, and returns both. The rules are
// withGate's: a stretch is charged only across intervals whose both ends were
// seen in the wait, no tick charges more than maxCharge, and the banked total
// never passes maxTotal.
func chargeWait(spent time.Duration, seenAt, now time.Time, inWait bool, maxCharge, maxTotal time.Duration) (time.Duration, time.Time) {
	switch {
	case inWait && seenAt.IsZero():
		// The first tick that found the wait: nothing before it is charged.
		seenAt = now
	case inWait && now.After(seenAt):
		spent += min(now.Sub(seenAt), maxCharge)
		seenAt = now
	case inWait:
		// A stamp at or after now is a clock that stepped back. Drop the
		// stretch rather than charge a negative interval, which would give back
		// time this sling was already credited with; the next tick starts it
		// again, and what is not charged counts toward the stop (gt-vfwuc).
		seenAt = time.Time{}
	default:
		// The wait is over: what it ran past the last tick that saw it open is
		// not charged, because the guard cannot tell that from work the seat
		// did after it.
		seenAt = time.Time{}
	}
	// The ceiling binds whatever the record held, a state file written before
	// it existed included.
	return min(spent, maxTotal), seenAt
}

// sameWait reports whether two records carry the same gate, escalation and
// restart accounting, so a tick writes the state only when one moved. The gate
// read's last answer and failure run are part of the accounting: a failed read
// that lengthens the run must reach the next tick, or the carry never expires.
func (s overrunState) sameWait(o overrunState) bool {
	return s.GateIn == o.GateIn && s.GateReadFailures == o.GateReadFailures &&
		s.GateSeenAt.Equal(o.GateSeenAt) && s.GateSpent == o.GateSpent && s.GateSlingAt.Equal(o.GateSlingAt) &&
		s.EscalationSeenAt.Equal(o.EscalationSeenAt) && s.EscalationSpent == o.EscalationSpent &&
		s.EscalationSlingAt.Equal(o.EscalationSlingAt) &&
		s.RestartSlingAt.Equal(o.RestartSlingAt) && s.RestartedAt.Equal(o.RestartedAt)
}

// overrunState is what the guard remembers about one bead between ticks.
//
// The stop's progress is kept here too, one stamp per step, because every
// step after the pause outlives the seat's visibility: once the session is
// gone the seat is no longer a working seat, so a step that fails is retried
// from this record (pendingOverrunStops), not from the seat list.
type overrunState struct {
	Bead string `json:"bead"`
	// NudgedAt is when the guard marked the seat at the nudge threshold. A
	// stamp not before the current clock suppresses a second mark, so each run
	// of the bead is marked once.
	NudgedAt time.Time `json:"nudged_at,omitzero"`

	// GateSlingAt is the sling the gate accounting below belongs to, GateSpent
	// the gate time charged for it (at most the configured gate_allowance, or
	// overrunMaxGateTotal when it is unset, for the sling), and GateSeenAt —
	// when set — the last tick that saw the seat inside its
	// gate (see withGate). The guard measures the sling's elapsed time as
	// (now - slung) - GateSpent, so the time a seat waits inside its own gate
	// does not count toward the 12 and 20 minute rule, and a gate that never
	// returns does (gt-vfwuc).
	GateSlingAt time.Time     `json:"gate_sling_at,omitzero"`
	GateSpent   time.Duration `json:"gate_spent,omitempty"`
	GateSeenAt  time.Time     `json:"gate_seen_at,omitzero"`

	// GateIn is the answer of the last successful gate read: whether the seat
	// was inside its own gate. GateReadFailures counts the consecutive failed
	// reads since the last successful one, so a short run of them carries that
	// answer instead of counting all of the seat's time (recordGateRead,
	// carriedGate). A record written before these fields existed holds false
	// and zero, which carries nothing.
	GateIn           bool `json:"gate_in,omitempty"`
	GateReadFailures int  `json:"gate_read_failures,omitempty"`

	// EscalationSlingAt, EscalationSpent and EscalationSeenAt are the same
	// accounting for the time the seat waits on an escalation it raised
	// (withEscalation), capped per sling by overrunMaxEscalationTotal.
	EscalationSlingAt time.Time     `json:"escalation_sling_at,omitzero"`
	EscalationSpent   time.Duration `json:"escalation_spent,omitempty"`
	EscalationSeenAt  time.Time     `json:"escalation_seen_at,omitzero"`

	// RestartSlingAt is the sling RestartedAt belongs to, and RestartedAt the
	// restart of that sling's seat the guard has adopted as the start of the
	// clock (withRestart). They are saved here because the intent record keeps
	// restart stamps only for the supervisor's budget window, shorter than a
	// run the guard polices (gt-y9lkx). RestartResets counts the restarts
	// adopted for that sling, against overrunMaxRestartClocks.
	RestartSlingAt time.Time `json:"restart_sling_at,omitzero"`
	RestartedAt    time.Time `json:"restarted_at,omitzero"`
	RestartResets  int       `json:"restart_resets,omitempty"`

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
	// StopReason is why the stop began, when it was not the 20 minute rule. It
	// is saved so the stop resumed on a later tick — where the seat carries no
	// reason — hands the same reason back, and so the needs-split attention
	// item can name it after the stop has finished (collectOverrunStopped).
	StopReason string `json:"stop_reason,omitempty"`

	// StopBeganAt is when the pause marker went down, SessionStoppedAt when
	// the session was confirmed gone, CheckpointedAt when the worktree
	// checkpoint step finished. RunEndedAt is when the run's end was recorded
	// on the bead (events.TypeOverrunStop), stamped so a stop retried on later
	// ticks — a failing hand-back — writes the record once rather than a later
	// end every tick, which could creep past the dispatch it ends. It is the
	// boundary the dispatch clock and the stopped-run skip share
	// (stoppedInsideRun). StoppedAt is set last, once the bead is handed back:
	// it marks the stop finished, and a stop that recorded no end — one
	// abandoned when the seat was taken back, or a scheduled run — leaves it as
	// the only record of the sling it stopped.
	StopBeganAt      time.Time `json:"stop_began_at,omitzero"`
	SessionStoppedAt time.Time `json:"session_stopped_at,omitzero"`
	CheckpointedAt   time.Time `json:"checkpointed_at,omitzero"`
	RunEndedAt       time.Time `json:"run_ended_at,omitzero"`
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

	// SavedAt is when the save step (overrunStop, step 1b) succeeded, or gave
	// up; a stop resumed later does not save again once it is set. SavedHead is
	// the branch head that save pushed (empty when it had nothing to push),
	// SaveCommitted says it made the WIP commit. A refused save (step 1b, or the
	// late re-save of step 2a) records SaveFailure, why its push or commit was
	// refused, and counts in SaveFailures: the stop holds and the next tick
	// saves again. A success clears both. At overrunSaveMaxAttempts consecutive
	// refusals the stop gives up on the save (SaveGaveUp): the seat is parked
	// with SaveFailure as the reason, and the sandbox is kept. A late check
	// (step 2a) that cannot read the worktree counts in the same SaveFailures
	// and sets SaveFailedLate, so the hand-back note says it was a late check.
	SavedAt        time.Time `json:"saved_at,omitzero"`
	SavedHead      string    `json:"saved_head,omitempty"`
	SaveCommitted  bool      `json:"save_committed,omitempty"`
	SaveFailure    string    `json:"save_failure,omitempty"`
	SaveFailures   int       `json:"save_failures,omitempty"`
	SaveFailedLate bool      `json:"save_failed_late,omitempty"`
	SaveGaveUp     bool      `json:"save_gave_up,omitempty"`

	// OriginReadFailures counts the failed reads of origin in the sandbox-gone
	// path of this stop (overrunStop, step 2b): a read that returned an error,
	// a record naming no branch, and a worktree that did not resolve. It is
	// capped at overrunOriginReadMaxAttempts and cleared when a later read of
	// origin succeeds (or is no longer needed). Short of the cap the stop holds
	// the bead with a record of how long it has tried; at the cap it gives up
	// on the read, keeps the sandbox's record and hands the bead back with the
	// reason and a high-severity item.
	OriginReadFailures int `json:"origin_read_failures,omitempty"`

	// OriginReadRaiseFailures counts the failed raises of that high-severity
	// item, one per tick once OriginReadFailures reached its cap. A failed
	// raise holds the bead so it is never handed back without its item, up to
	// overrunOriginRaiseMaxAttempts; past that bound the bead is handed back
	// anyway, since a failed raise is already recorded in the town feed as a
	// dropped escalation and the hand-back note carries the reason. It is
	// cleared with OriginReadFailures.
	OriginReadRaiseFailures int `json:"origin_read_raise_failures,omitempty"`

	// SandboxDiverged is set when the sandbox's push was refused because its
	// branch diverged from origin (overrunStop, step 2b). It holds the hand-back
	// sentence naming the sandbox and both heads. Once set, the stop never asks
	// the sandbox again: a later read that finds the sandbox gone must not
	// delete the commits only it holds, so the sandbox, the worktree and the
	// pause are kept, and the bead is handed back with this note. The note lasts
	// for that stop: once it has handed the bead back (StoppedAt is set), the
	// next stop starts from a fresh state (overrunStop, overrunScheduledStop),
	// which clears it; overrunStop then asks the sandbox again.
	SandboxDiverged string `json:"sandbox_diverged,omitempty"`

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

// stoppedInsideRun reports whether slung dates a dispatch of the run this
// bead's stop already ended, which the guard must not stop a second time on the
// same clock. The boundary is RunEndedAt, the end the stop recorded on the bead
// and the one the dispatch clock reads: a dispatch strictly before it belongs to
// the ended run, one dated at or after it is the new run's and is policed. The
// stop writes StoppedAt after the hand-back, so a re-sling landing in between is
// the new run's too (gt-lfszo). A stop that recorded no end — a scheduled run,
// whose close is its end, or one abandoned when the seat was taken back — leaves
// StoppedAt as the only record that its sling was stopped.
func (s overrunState) stoppedInsideRun(slung time.Time) bool {
	if !s.RunEndedAt.IsZero() {
		return slung.Before(s.RunEndedAt)
	}
	return !s.StoppedAt.IsZero() && !s.StoppedAt.Before(slung)
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
