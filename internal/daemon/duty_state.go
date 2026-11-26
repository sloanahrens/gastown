package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// dutyStateName is the state file's name under the town's daemon directory.
const dutyStateName = "duty_state.json"

// DutyState is the reconciler's duty record at one instant: when the
// observations were taken and one record per observation, in their order.
type DutyState struct {
	At     time.Time
	Duties []DutyRecord
}

// DutyRecord is one duty's judgement: its name, verdict, last good result and
// the age and limit in whole seconds. LastGood is zero with no result.
type DutyRecord struct {
	Name         string
	Verdict      reconcile.Verdict
	LastGood     time.Time
	AgeSeconds   int64
	LimitSeconds int64
}

// dutyStatePath is where the duty state record lives under the town root.
func dutyStatePath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", dutyStateName)
}

// WriteDutyState writes the duty state record for at and obs to
// daemon/duty_state.json under townRoot, replacing any previous record
// atomically so a reader sees one whole record or the other.
func WriteDutyState(townRoot string, at time.Time, obs []reconcile.Observation, limits map[string]time.Duration) error {
	// An empty root would name a path relative to the process's working
	// directory, so the record would land somewhere no reader looks.
	if townRoot == "" {
		return errors.New("write duty state: no town root")
	}
	path := dutyStatePath(townRoot)
	if err := atomicfile.EnsureDirAndWriteJSON(path, newDutyState(at, obs, limits)); err != nil {
		return fmt.Errorf("write duty state to %s: %w", path, err)
	}
	return nil
}

// newDutyState turns the observations into the record the state file holds.
// Each record takes its name, verdict, last good time and age from its
// observation and its limit from limits keyed by that name, so a missing entry
// records a zero limit. The order is the observations'.
func newDutyState(at time.Time, obs []reconcile.Observation, limits map[string]time.Duration) DutyState {
	state := DutyState{At: at, Duties: make([]DutyRecord, 0, len(obs))}
	for _, o := range obs {
		state.Duties = append(state.Duties, DutyRecord{
			Name:         o.Duty,
			Verdict:      o.Verdict,
			LastGood:     o.LastGood,
			AgeSeconds:   int64(o.Age / time.Second),
			LimitSeconds: int64(limits[o.Duty] / time.Second),
		})
	}
	return state
}
