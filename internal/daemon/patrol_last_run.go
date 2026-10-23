package daemon

import (
	"encoding/json"
	"errors"
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

// errPatrolLastRunCorrupt marks a last-run file that was read but is not valid
// JSON. It is the one load failure an in-memory completion time cannot explain
// away: a write that could not create a record leaves the file missing or its
// directory unusable, never garbled.
var errPatrolLastRunCorrupt = errors.New("corrupt last-run file")

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
		return time.Time{}, false, fmt.Errorf("parse %s: %w: %w", patrolLastRunFileName, errPatrolLastRunCorrupt, err)
	}

	lastRun, ok := state.LastRun[patrol]
	if !ok || lastRun.IsZero() {
		return time.Time{}, false, nil
	}
	return lastRun, true, nil
}

// patrolLastRunMemory is a patrol's last completion time as this process
// recorded it. It outranks the persisted record when a save failed, and stands
// in for that record when the failed save could not create a file at all.
//
// The pass writes it from its own goroutine while the heartbeat reads it, so
// the access is guarded.
type patrolLastRunMemory struct {
	mu      sync.Mutex
	lastRun time.Time
}

func (m *patrolLastRunMemory) record(at time.Time) {
	m.mu.Lock()
	m.lastRun = at
	m.mu.Unlock()
}

// last returns the completion time recorded so far, zero before the first one.
func (m *patrolLastRunMemory) last() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRun
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
	// schedule. A corrupt file also warns when an in-memory completion time
	// decided the comparison instead.
	warn string
}

// evaluatePatrolDue decides whether a patrol should run now, given its
// persisted last-run time on disk. inMemory is the latest completion this
// process itself recorded (which can be newer than the disk record when a
// previous write failed, and can be the only record when the write left no
// readable file behind at all); pass the zero time.Time when the caller keeps
// no in-memory record of its own.
func evaluatePatrolDue(townRoot, patrol string, inMemory, now time.Time, interval time.Duration) patrolDueDecision {
	lastRun, found, err := loadPatrolLastRun(townRoot, patrol)

	// A cycle this process ran outranks the file: it can be newer than the
	// record when the write failed, and — the load above having left lastRun
	// zero — it stands in for the record entirely when the write could not
	// create one. Without the second half a town whose daemon directory cannot
	// be written would read "due" on every check and re-run the patrol there,
	// once per check, for as long as it stayed broken (jev_dedupe's passes make
	// network calls, gt-4nqej). A read failure of that kind is the failed write
	// seen again, and was logged when it happened, so nothing is warned.
	//
	// A corrupt file is not that failure: it is a broken record in its own
	// right, and every caller that passes an in-memory time would otherwise
	// lose the warning. The in-memory time still decides whether the patrol is
	// due; the warning rides with the decision.
	var corruptWarn string
	if !inMemory.IsZero() && inMemory.After(lastRun) {
		if errors.Is(err, errPatrolLastRunCorrupt) {
			corruptWarn = fmt.Sprintf("last-run state unreadable (%v)", err)
		}
		lastRun, found, err = inMemory, true, nil
	}

	return patrolDueFromLastRun(lastRun, found, err, corruptWarn, now, interval)
}

// patrolDueFromLastRun is evaluatePatrolDue's decision over a record the
// caller has already read. A caller that reads the record for a second purpose
// as well — rebuild_gt holds a request for a minimum interval before a forced
// run — decides both from one read, so the two cannot disagree about whether
// the state was readable.
func patrolDueFromLastRun(lastRun time.Time, found bool, loadErr error, warn string, now time.Time, interval time.Duration) patrolDueDecision {
	switch {
	case loadErr != nil:
		return patrolDueDecision{
			due:  true,
			note: "running the check because the last-run state cannot be read",
			warn: fmt.Sprintf("last-run state unreadable (%v)", loadErr),
		}
	case !found:
		return patrolDueDecision{due: true, note: "no last-run record"}
	}

	elapsed := now.Sub(lastRun).Round(time.Minute)
	if elapsed >= interval {
		return patrolDueDecision{due: true, note: fmt.Sprintf("last run %s ago, interval %v", elapsed, interval), warn: warn}
	}
	return patrolDueDecision{note: fmt.Sprintf("last run %s ago, interval %v", elapsed, interval), warn: warn}
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
// patrol (60m+) checking often enough to catch up quickly
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

// recordPatrolRun records a patrol's completion time in memory and on disk.
//
// The in-memory time is set even when the write fails: with no record on disk
// every heartbeat reads the patrol as due and opens another pass, each reaching
// the network for work its cursor has not paid for yet (gt-4nqej). A restart
// then reads the missing record as "run the pass", the safe direction for a
// patrol.
func (d *Daemon) recordPatrolRun(mem *patrolLastRunMemory, patrol string, at time.Time) {
	mem.record(at)
	if err := savePatrolLastRun(d.config.TownRoot, patrol, at); err != nil {
		d.logger.Printf("%s: WARNING: cannot persist last-run time (%v) — "+
			"the next daemon start will run the pass again", patrol, err)
	}
}
