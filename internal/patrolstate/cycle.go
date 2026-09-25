package patrolstate

// Role-scoped session cycling (claude-8w7 step 3).
//
// A patrol role that respawns its own session on a quiet boundary needs two
// facts that must not live in the agent's context, because the respawn wipes
// it:
//
//  1. How did this cycle's await-signal wait end? `gt mol await-signal`
//     writes a WaitOutcome to <role dir>/.runtime/await-signal-last.json on
//     every return.
//  2. How many cycles has this session completed? `gt patrol report` keeps a
//     CycleState in <role dir>/.runtime/session-cycles.json, keyed by the
//     runtime session ID, so a fresh session (respawn, daemon restart, crash)
//     starts again at zero without anyone resetting it.
//
// Both files are written only by Go, atomically, and read back with a strict
// schema version; an unreadable file reads as "nothing recorded", which only
// delays a respawn and never causes one. The files are role-agnostic: the
// witness uses them today, and the deacon can reuse them (claude-9jq tier 4)
// by pointing the same calls at its own directory.
//
// This is deliberately not ResetCounter's field-preserving rewrite: those
// state.json files belong to the agent, while these belong to Go alone.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

const (
	runtimeDirName   = ".runtime"
	waitOutcomeFile  = "await-signal-last.json"
	cycleStateFile   = "session-cycles.json"
	cycleFileVersion = 1
)

// WaitOutcome records how one await-signal wait ended.
type WaitOutcome struct {
	Version int `json:"version"`
	// Reason is "signal" or "timeout".
	Reason string `json:"reason"`
	// Timeout is the full backoff window the wait was computed with, even when
	// an interrupted wait resumed with only the remainder of it.
	Timeout time.Duration `json:"timeout_ns"`
	// BackoffMax is the configured cap; zero when no cap was set.
	BackoffMax time.Duration `json:"backoff_max_ns"`
	// AtCap is true when Timeout had reached BackoffMax.
	AtCap bool `json:"at_cap"`
	// IdleCycles and EffortLevel are what await-signal reported to the agent.
	IdleCycles  int    `json:"idle_cycles"`
	EffortLevel string `json:"effort_level"`
	// SessionID is the runtime session that waited; empty when unknown.
	SessionID string    `json:"session_id,omitempty"`
	At        time.Time `json:"at"`
}

// TimedOutAtCap reports whether the wait ran its whole window at the backoff
// cap without a signal: the quiet boundary a session may respawn on.
func (w *WaitOutcome) TimedOutAtCap() bool {
	return w != nil && w.Reason == "timeout" && w.AtCap
}

// WaitOutcomePath returns where a role's last wait outcome lives.
func WaitOutcomePath(dir string) string {
	return filepath.Join(dir, runtimeDirName, waitOutcomeFile)
}

// WriteWaitOutcome stamps and atomically writes o under dir.
func WriteWaitOutcome(dir string, o WaitOutcome) error {
	o.Version = cycleFileVersion
	return atomicfile.EnsureDirAndWriteJSON(WaitOutcomePath(dir), o)
}

// ReadWaitOutcome returns the last wait outcome under dir, or nil when none
// was recorded. A file that cannot be parsed, or has an unknown version, is
// an error; callers treat it as "no wait recorded".
func ReadWaitOutcome(dir string) (*WaitOutcome, error) {
	var o WaitOutcome
	found, err := readVersioned(WaitOutcomePath(dir), &o, func() int { return o.Version })
	if err != nil || !found {
		return nil, err
	}
	return &o, nil
}

// CycleState is the per-session cycle counter.
type CycleState struct {
	Version int `json:"version"`
	// SessionID is the runtime session the count belongs to. A report from any
	// other session starts the count again.
	SessionID string `json:"session_id"`
	// Cycles is how many patrol reports this session has completed.
	Cycles int `json:"cycles"`
	// LastWaitAt is the At of the last wait outcome a report consumed, so one
	// wait never counts for two reports.
	LastWaitAt time.Time `json:"last_wait_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CycleStatePath returns where a role's cycle counter lives.
func CycleStatePath(dir string) string {
	return filepath.Join(dir, runtimeDirName, cycleStateFile)
}

// LoadCycleState returns the counter under dir; a missing file is the zero
// state. An unreadable or unknown-version file is an error, and the caller
// starts from the zero state (the count restarts, which only delays a
// respawn).
func LoadCycleState(dir string) (CycleState, error) {
	var s CycleState
	found, err := readVersioned(CycleStatePath(dir), &s, func() int { return s.Version })
	if err != nil || !found {
		return CycleState{}, err
	}
	return s, nil
}

// SaveCycleState stamps and atomically writes s under dir.
func SaveCycleState(dir string, s CycleState) error {
	s.Version = cycleFileVersion
	return atomicfile.EnsureDirAndWriteJSON(CycleStatePath(dir), s)
}

// CycleStep is what one patrol report learns: the counter to persist and
// whether the wait that ended this cycle timed out at the cap.
type CycleStep struct {
	State CycleState
	// FreshSession is true when the count restarted for a new session.
	FreshSession bool
	// Wait is this cycle's wait outcome, or nil when none was recorded for it
	// (already consumed, from another session, or never written).
	Wait *WaitOutcome
}

// AdvanceCycle counts one completed patrol cycle for sessionID. The count
// restarts when the stored state belongs to another session. wait is
// attributed to this cycle only when it was not consumed by an earlier report
// and, when both sides know their session, came from this session.
func AdvanceCycle(prev CycleState, sessionID string, wait *WaitOutcome, now time.Time) CycleStep {
	step := CycleStep{State: prev}
	if prev.SessionID != sessionID {
		step.FreshSession = true
		// LastWaitAt is a watermark over the one outcome file, not a
		// per-session fact: keep it so a predecessor's wait is never reused.
		step.State = CycleState{SessionID: sessionID, LastWaitAt: prev.LastWaitAt}
	}
	step.State.Cycles++
	step.State.UpdatedAt = now

	if wait != nil && wait.At.After(prev.LastWaitAt) &&
		(sessionID == "" || wait.SessionID == "" || wait.SessionID == sessionID) {
		step.Wait = wait
		step.State.LastWaitAt = wait.At
	}
	return step
}

// CycleBoundary decides whether a session that has completed cycles cycles,
// whose latest wait was wait, should respawn now. minCycles gates the idle-cap
// trigger; maxCycles is the backstop (<= 0 disables it). reason names the
// trigger when respawn is true, and why the session is kept otherwise.
func CycleBoundary(cycles int, wait *WaitOutcome, minCycles, maxCycles int) (respawn bool, reason string) {
	if wait.TimedOutAtCap() && cycles >= minCycles {
		return true, fmt.Sprintf("wait timed out at the %v idle cap after %d cycles", wait.BackoffMax, cycles)
	}
	if maxCycles > 0 && cycles >= maxCycles {
		return true, fmt.Sprintf("backstop: %d cycles this session (max %d)", cycles, maxCycles)
	}
	switch {
	case wait.TimedOutAtCap():
		return false, fmt.Sprintf("cycle %d; wait timed out at the idle cap, but a respawn needs %d cycles this session", cycles, minCycles)
	case wait == nil:
		return false, fmt.Sprintf("cycle %d; no await-signal outcome recorded for this cycle", cycles)
	case wait.Reason == "timeout":
		return false, fmt.Sprintf("cycle %d; wait timed out below the idle cap (%v)", cycles, wait.Timeout)
	default:
		return false, fmt.Sprintf("cycle %d; wait woke on a signal", cycles)
	}
}

// readVersioned decodes path into v and checks its version. found is false
// for a missing file.
func readVersioned(path string, v any, version func() int) (found bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}
	if got := version(); got != cycleFileVersion {
		return false, fmt.Errorf("%s: unknown version %d", path, got)
	}
	return true, nil
}
