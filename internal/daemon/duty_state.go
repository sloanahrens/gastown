package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/reconcile"
)

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
