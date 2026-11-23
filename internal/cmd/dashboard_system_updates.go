package cmd

import (
	"errors"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/update"
)

// systemUpdatesHistoryRuns is how many finished gt update runs the section reads
// for its rolls, so the rolls of the newest commit rows are among them.
const systemUpdatesHistoryRuns = 50

// systemUpdatesPipelineEvery is how often the gastown repository's pipeline runs
// are re-read. It is the hub's Deploys cadence, which newDashboardHub sets from
// this constant, because one read is two Forgejo list calls and the runs move
// far slower than the section is built.
const systemUpdatesPipelineEvery = time.Minute

// newSystemUpdatesPipeline gates read so it runs at most once per every, timed
// by now: a section built inside the window reuses what the last read
// published, so the section's faster poll costs the repository nothing. have
// records that a read happened, so a nil result is cached like any other.
func newSystemUpdatesPipeline(read func() *dashboard.GastownPipeline, now func() time.Time, every time.Duration) func() *dashboard.GastownPipeline {
	var mu sync.Mutex
	var last *dashboard.GastownPipeline
	var at time.Time
	var have bool
	return func() *dashboard.GastownPipeline {
		mu.Lock()
		defer mu.Unlock()
		readAt := now()
		if !have || readAt.Sub(at) >= every {
			last = read()
			at = readAt
			have = true
		}
		return last
	}
}

// systemUpdatesPipelineFor is the section's pipeline read for the gastown rig's
// remote: nil when no remote is named (the rig is not cut over, so the section
// joins no runs), and a read that reports a failure, with no runs, when the
// remote cannot be looked up or parsed, so the section raises RunsUnknown instead
// of reading like a healthy town with no runs. build makes the throttled read
// for owner/repo (om finding 46a9e85f).
func systemUpdatesPipelineFor(remote string, remoteErr error, build func(repo string) func() *dashboard.GastownPipeline, logf func(format string, args ...any)) func() *dashboard.GastownPipeline {
	failed := func(why error) func() *dashboard.GastownPipeline {
		logf("dashboard: the System updates pipeline has no gastown repository to read: %v", why)
		return func() *dashboard.GastownPipeline {
			return &dashboard.GastownPipeline{Runs: []dashboard.PipelineRun{}, Error: "remote"}
		}
	}
	if remoteErr != nil {
		return failed(remoteErr)
	}
	if remote == "" {
		return nil
	}
	owner, repo, err := land.RepoFromRemoteURL(remote)
	if err != nil {
		return failed(err)
	}
	return build(owner + "/" + repo)
}

// systemUpdates is the feed's System updates section: the tier sweeps the sweep
// reader holds joined with the finished rolls in the update history and the
// gastown repository's post-land and images runs. A sweep log or history that
// cannot be read marks the section unavailable, so the rows it did join are not
// taken for every commit's full record. A nil pipeline read joins no runs.
func systemUpdates(townRoot string, sweeps *tierSweepReader, pipeline func() *dashboard.GastownPipeline, now time.Time) *dashboard.SystemUpdates {
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
	var pipelineRead *dashboard.GastownPipeline
	if pipeline != nil {
		pipelineRead = pipeline()
	}
	su := dashboard.NewSystemUpdates(ts.Sweeps, pipelineRead, rolls, ts.Unavailable || err != nil)
	// The update reader answers nil for both no run and a record it cannot read,
	// so the record is read here to tell the page which one a missing roll is.
	su.RollUnknown = updateRecordUnreadable(update.Dir(townRoot))
	// A pipeline read that failed with no good runs behind it is a reading that
	// failed: the run stages are unknown, not absent (gt-nhdxkq).
	su.RunsUnknown = pipelineRead != nil && pipelineRead.Error != "" && len(pipelineRead.Runs) == 0
	return su
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
