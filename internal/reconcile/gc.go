package reconcile

import (
	"sort"
	"time"
)

// GCObservations judges each database the server lists by its own last gc: one
// Observation per listed name, in name order, named duty.Name, a colon and the
// database.
//
// A listed database with no lastGC entry has never been gc'd, so it is judged
// as ErrNoResult and reads overdue. Entries for databases the server does not
// list are ignored: a leftover key is no evidence about a listed database, and
// its own database may no longer exist.
func GCObservations(duty Duty, now time.Time, lastGC map[string]time.Time, databases []string) []Observation {
	names := append([]string(nil), databases...)
	sort.Strings(names)

	observations := make([]Observation, 0, len(names))
	for _, db := range names {
		d := duty
		d.Name = duty.Name + ":" + db
		lastGood, ok := lastGC[db]
		var readErr error
		if !ok {
			readErr = ErrNoResult
		}
		observations = append(observations, Evaluate(d, now, lastGood, readErr))
	}
	return observations
}
