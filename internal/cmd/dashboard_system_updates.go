package cmd

import (
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
	return dashboard.NewSystemUpdates(ts.Sweeps, nil, rolls, ts.Unavailable || err != nil)
}
