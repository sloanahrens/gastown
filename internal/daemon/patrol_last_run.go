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
