package deacon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// cycleObservation records the heartbeat cycle last seen and when it was first
// seen, so a later observation of the same cycle can be dated from it.
type cycleObservation struct {
	Cycle      int64     `json:"cycle"`
	ObservedAt time.Time `json:"observed_at"`
}

// CycleObservationPath returns the path to the persisted cycle observation.
// It lives under the town's runtime directory alongside the poller's PID files.
func CycleObservationPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "deacon-cycle.json")
}

// CycleAge reports how long the given heartbeat cycle has gone unchanged,
// recording it when it is new or when nothing was recorded yet; the call that
// records a cycle returns zero.
//
// The daemon dates cycles in memory to decide nudges and restarts (gt-t3cw);
// this is the same measure for a process that has no memory between runs, so
// it dates a cycle from the last time something looked. An unreadable
// observation file counts as no observation.
func CycleAge(townRoot string, cycle int64, now time.Time) time.Duration {
	obs, ok := readCycleObservation(townRoot)
	if !ok || obs.Cycle != cycle {
		writeCycleObservation(townRoot, cycleObservation{Cycle: cycle, ObservedAt: now})
		return 0
	}
	return now.Sub(obs.ObservedAt)
}

func readCycleObservation(townRoot string) (cycleObservation, bool) {
	data, err := os.ReadFile(CycleObservationPath(townRoot)) //nolint:gosec // G304: path is constructed from trusted townRoot
	if err != nil {
		return cycleObservation{}, false
	}

	var obs cycleObservation
	if err := json.Unmarshal(data, &obs); err != nil {
		return cycleObservation{}, false
	}
	if obs.ObservedAt.IsZero() {
		return cycleObservation{}, false
	}
	return obs, true
}

func writeCycleObservation(townRoot string, obs cycleObservation) {
	// Best-effort: status output does not depend on the write succeeding, it
	// only means the next run re-baselines.
	_ = atomicfile.EnsureDirAndWriteJSON(CycleObservationPath(townRoot), obs)
}
