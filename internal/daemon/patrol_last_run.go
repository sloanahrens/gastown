package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// Patrol last-run bookkeeping: the persisted completion time of a patrol's last
// cycle, so its period is wall-clock rather than "time since this daemon
// process started".
//
// In-process tickers have no memory, so a patrol whose tick interval *is* its
// intended run cadence restarts its countdown on every daemon restart and is
// starved outright on a host that restarts the daemon more often than the
// interval (gt-ima2). Pair a coarse check tick with a due-ness decision made
// from the last-run time recorded here.

const patrolLastRunFileName = "patrol_last_run.json"

// patrolLastRunWriteMu guards the read-modify-write of the shared file, so two
// patrols recording a run at once cannot drop each other's entry.
var patrolLastRunWriteMu sync.Mutex

// patrolLastRunPath returns the path of the daemon's patrol last-run file.
func patrolLastRunPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", patrolLastRunFileName)
}

// patrolLastRunState is the on-disk shape: one completion time per patrol.
type patrolLastRunState struct {
	LastRun map[string]time.Time `json:"last_run"`
}

// loadPatrolLastRun returns a patrol's last recorded completion time, and false
// when none has ever been recorded. An unreadable or corrupt file is an error
// the caller reports and then runs the patrol anyway: reading broken state as
// "not due" is silent starvation (gt-ima2).
func loadPatrolLastRun(townRoot, patrol string) (time.Time, bool, error) {
	data, err := os.ReadFile(patrolLastRunPath(townRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}

	var state patrolLastRunState
	if err := json.Unmarshal(data, &state); err != nil {
		return time.Time{}, false, fmt.Errorf("parse %s: %w", patrolLastRunFileName, err)
	}

	lastRun, ok := state.LastRun[patrol]
	if !ok || lastRun.IsZero() {
		return time.Time{}, false, nil
	}
	return lastRun, true, nil
}

// patrolDueDecision is one due-ness evaluation for a ticker-driven patrol
// whose run interval is enforced against a persisted last-run time rather
// than an in-process countdown that a restart resets (gt-ima2).
type patrolDueDecision struct {
	// due is whether the patrol should run now.
	due bool
	// note explains the decision for the log. Never empty.
	note string
	// warn is set when the decision came from a broken last-run record rather
	// than from a comparison. A broken record runs the patrol *and* says so:
	// silent skipping is the failure being fixed, and a run without a word
	// would hide a corrupt state file behind a patrol that looks merely on
	// schedule.
	warn string
}

// evaluatePatrolDue decides whether a patrol should run now, given its
// persisted last-run time on disk. inMemory is the latest completion this
// process itself recorded (which can be newer than the disk record when a
// previous write failed); pass the zero time.Time when the caller keeps no
// in-memory record of its own.
func evaluatePatrolDue(townRoot, patrol string, inMemory, now time.Time, interval time.Duration) patrolDueDecision {
	lastRun, found, err := loadPatrolLastRun(townRoot, patrol)
	switch {
	case err != nil:
		return patrolDueDecision{
			due:  true,
			note: "running the check because the last-run state cannot be read",
			warn: fmt.Sprintf("last-run state unreadable (%v)", err),
		}
	case !found:
		return patrolDueDecision{due: true, note: "no last-run record"}
	}

	// A cycle this process ran can be newer than the file when the write
	// failed; take the later of the two so a known completion is not
	// repeated on the next check.
	if inMemory.After(lastRun) {
		lastRun = inMemory
	}

	elapsed := now.Sub(lastRun).Round(time.Minute)
	if elapsed >= interval {
		return patrolDueDecision{due: true, note: fmt.Sprintf("last run %s ago, interval %v", elapsed, interval)}
	}
	return patrolDueDecision{note: fmt.Sprintf("last run %s ago, interval %v", elapsed, interval)}
}

// shortPatrolCheckTick returns a check cadence for a due-ness-gated patrol
// (evaluatePatrolDue) that is shorter than its run interval.
//
// A ticker set to the run interval itself starves the patrol to half its
// intended rate once a cycle takes non-negligible wall-clock time: last-run is
// recorded at cycle END, so the elapsed time the NEXT tick sees is
// interval-minus-cycle-duration. Once that drops below interval, the tick
// reads as "not due" and is skipped — and the tick after that repeats the
// pattern, so a patrol runs on every other tick instead of every tick (crew
// review of gt-gxpwc: a 15m interval with a 2m cycle produced 4 runs in 8
// ticks instead of 8). A quarter of the interval keeps that rounding error
// from ever accumulating to a full skip; the 5m cap keeps a long-interval
// patrol (main_branch_test, 60m+) checking often enough to catch up quickly
// after a restart, matching compactor_dog's existing fixed 15m tick against
// its 24h interval.
func shortPatrolCheckTick(interval time.Duration) time.Duration {
	tick := interval / 4
	if tick > 5*time.Minute {
		return 5 * time.Minute
	}
	if tick < time.Minute {
		return time.Minute
	}
	return tick
}

// savePatrolLastRun records a patrol's completion time, preserving the entries
// of other patrols.
func savePatrolLastRun(townRoot, patrol string, at time.Time) error {
	patrolLastRunWriteMu.Lock()
	defer patrolLastRunWriteMu.Unlock()

	path := patrolLastRunPath(townRoot)

	// A corrupt file is replaced rather than failing the write: the caller has
	// already run the patrol, and refusing to write would leave a record that
	// can never be repaired.
	var state patrolLastRunState
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	if state.LastRun == nil {
		state.LastRun = map[string]time.Time{}
	}
	state.LastRun[patrol] = at

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicfile.WriteJSON(path, state)
}
