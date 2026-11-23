package cmd

import (
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/update"
)

// systemUpdatesHistoryRuns is how many finished gt update runs the section reads
// for its rolls, so the rolls of the newest commit rows are among them.
const systemUpdatesHistoryRuns = 50

// systemUpdates is the feed's System updates section: the tier sweeps the sweep
// reader holds joined with the finished rolls in the update history. A sweep log
// or history that cannot be read marks the section unavailable, so the rows it
// did join are not taken for every commit's full record.
func systemUpdates(townRoot string, sweeps *tierSweepReader, now time.Time) *dashboard.SystemUpdates {
	ts := sweeps.read(now)
	runs, err := update.ReadHistory(update.Dir(townRoot), systemUpdatesHistoryRuns)
	rolls := make([]dashboard.SystemUpdateRoll, 0, len(runs))
	for _, run := range runs {
		rolls = append(rolls, dashboard.SystemUpdateRoll{
			From:      run.Target.InstalledSHA,
			To:        run.Target.SHA,
			Outcome:   string(run.Outcome),
			Current:   string(run.Current),
			StartedAt: run.StartedAt,
			EndedAt:   run.EndedAt,
		})
	}
	su := dashboard.NewSystemUpdates(ts.Sweeps, nil, rolls, ts.Unavailable || err != nil)
	// The update reader answers nil for both no run and a record it cannot read,
	// so the record is read here to tell the page which one a missing roll is.
	su.RollUnknown = updateRecordUnreadable(update.Dir(townRoot))
	return su
}

// updateRecordUnreadable reports whether the update job's state record exists
// but cannot be read. A missing record is the answer for a town that never ran
// the job, which is not unknown.
func updateRecordUnreadable(dir string) bool {
	_, err := (&update.Store{Dir: dir}).Read()
	return err != nil && !errors.Is(err, update.ErrNoState)
}
