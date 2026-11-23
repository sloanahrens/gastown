package cmd

import (
	"errors"
	"sync"
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
			Step:      rollStep(run),
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

// systemUpdatesPipelineEvery is the window the section's pipeline read reuses
// its last answer for: one minute, the Deploys block's own default
// (DeploysEvery, internal/dashboard/types.go). The section is built far more
// often than that, and each read costs two Forgejo list calls (gt-066cn5).
const systemUpdatesPipelineEvery = time.Minute

// newSystemUpdatesPipeline throttles read to one call per every. A call inside
// the window answers the previous result, a nil one included, so consecutive
// builds of the section do not each pay the read's two Forgejo list calls; the
// first call after the window reads again. Concurrent calls share the window
// rather than racing into a second read (gt-066cn5).
func newSystemUpdatesPipeline(read func() *dashboard.GastownPipeline, now func() time.Time, every time.Duration) func() *dashboard.GastownPipeline {
	var (
		mu     sync.Mutex
		reads  int
		last   *dashboard.GastownPipeline
		lastAt time.Time
	)
	return func() *dashboard.GastownPipeline {
		mu.Lock()
		defer mu.Unlock()
		at := now()
		if reads > 0 && at.Sub(lastAt) < every {
			return last
		}
		reads++
		last = read()
		lastAt = at
		return last
	}
}

// rollStep is the step a partial roll did not pass: the first step the run
// recorded failed or deferred, in plan order, so a run that swapped and
// restarted but could not verify reads "verify". A roll that ended any other
// way names none, because its outcome word already says where it stopped
// (gt-c8yqh9).
func rollStep(run update.HistoryLine) string {
	if run.Outcome != update.OutcomePartial {
		return ""
	}
	for _, step := range run.Steps {
		if step.Result == update.ResultFailed || step.Result == update.ResultDeferred {
			return string(step.Name)
		}
	}
	return ""
}

// updateRecordUnreadable reports whether the update job's state record exists
// but cannot be read. A missing record is the answer for a town that never ran
// the job, which is not unknown.
func updateRecordUnreadable(dir string) bool {
	_, err := (&update.Store{Dir: dir}).Read()
	return err != nil && !errors.Is(err, update.ErrNoState)
}
