package daemon

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// otherRunJob is the name of a sweep Job the renderer makes for the same rig at
// another sha: the Job a previous daemon's run left running, which holds the
// repo claim this run would rewrite (gt-pihhr4).
func otherRunJob(g *guardFixture) string {
	other := g.spec
	other.SHA = "fedcba9876543210fedcba9876543210fedcba98"
	other.RunID = tierSweepRunnerRunID(other.Rig, other.SHA, g.st.tiers)
	return sweeprunner.JobName(other)
}

// boundWaits makes the stage budget pass once the guard has listed the rig's Jobs
// more than listings times. A launch that waits on a Job the test keeps listed
// would otherwise never end, so a broken wait fails the test instead of hanging
// it. It wraps the command answer already set, so it is called after that.
func boundWaits(g *guardFixture, listings int) {
	passed := make(chan time.Time)
	g.d.tierSweepSeams.pollInterval = time.Millisecond
	g.d.tierSweepSeams.budgetTimer = func(time.Duration) (<-chan time.Time, func()) { return passed, func() {} }
	answer := g.cmds.answer
	seen, closed := 0, false
	g.cmds.answer = func(argv []string) ([]byte, []byte, error) {
		if slices.Contains(argv, "get") && slices.Contains(argv, "jobs") {
			seen++
			if seen > listings && !closed {
				closed = true
				close(passed)
			}
		}
		return answer(argv)
	}
}

// TestTierSweepRunningJobDefersWithoutVerdict: a stage that meets a running Job of
// the rig for another sha waits out its budget and then defers. No tier is
// recorded RED, no bead is filed, nothing is applied, and the one log line names
// the running Job and when the next run is due.
func TestTierSweepRunningJobDefersWithoutVerdict(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	running := otherRunJob(g)
	g.cluster.jobs = running + "\n"
	g.d.tierSweepSeams.pollInterval = time.Hour
	g.d.tierSweepSeams.budgetTimer = runnerBudgetSpent

	stage := g.cycle()

	if stage.ran {
		t.Fatalf("stage = %+v, want ran=false so the cycle defers rather than judging the tree", stage)
	}
	var busy *sweeprunner.ActiveJob
	if !errors.As(stage.err, &busy) || busy.Name != running {
		t.Fatalf("stage err = %v, want the *ActiveJob naming %q", stage.err, running)
	}
	if stage.reason != "" || strings.Contains(stage.output, tierSweepRunnerNoResult) {
		t.Errorf("stage = %+v, want no RED verdict and no %q reason", stage, tierSweepRunnerNoResult)
	}
	if g.applied() {
		t.Errorf("the launch applied objects while another run's Job was running: %v", g.cmds.ran)
	}
	if n := len(g.beads.created); n != 0 {
		t.Errorf("%d beads filed for a deferred cycle, want none", n)
	}
	retry := g.clock(t).Now().Add(tierSweepInterval(g.d.patrolConfig)).Format(time.RFC3339)
	wantLine := "sweep job " + running + " is still running after 1m0s; deferring the cycle, the next run is due " + retry
	if got := strings.Count(g.logs.String(), "sweep job "+running+" is still running"); got != 1 {
		t.Errorf("deferral lines naming the job = %d, want exactly one: %q", got, g.logs.String())
	}
	if !strings.Contains(g.logs.String(), wantLine) {
		t.Errorf("log = %q, want the line %q", g.logs.String(), wantLine)
	}
}

// TestTierSweepCycleDefersWhileAnotherRunsJobIsRunning: a whole rig cycle that
// meets another run's running Job defers its container stage. The cycle files no
// bead, records no RED for the tiers the Job would run, and its log names the
// Job and the next run. The host shell stage still runs and reports.
func TestTierSweepCycleDefersWhileAnotherRunsJobIsRunning(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	cfg := runnerTestConfig(t)
	cfg.Backend = config.TierSweepRunnerKubernetes
	const shellSummary = "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /out/logs)\n"
	// The same wiring as runnerCycleDaemon, with the log kept for the assertions.
	d, _, bd, logs := newTierSweepDaemon(t, now, "gastown")
	d.patrolConfig.Patrols.TierSweep.Runner = cfg
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return runnerTestSHA, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.tierSweepSeams.run = func(ctx context.Context, dir string, st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return d.tierSweepRunStage(ctx, dir, st)
		}
		return tierSweepStageResult{output: shellSummary, exitCode: 0, ran: true}
	}
	other := tierSweepStage{tiers: []string{"integration", "race"}}
	running := sweeprunner.JobName(sweeprunner.Spec{Rig: "gastown", SHA: "fedcba9876543210fedcba9876543210fedcba98", RunID: tierSweepRunnerRunID("gastown", "fedcba9876543210fedcba9876543210fedcba98", other.tiers)})
	cmds := &runnerFakeCmds{answer: func(argv []string) ([]byte, []byte, error) {
		if slices.Contains(argv, "get") && slices.Contains(argv, "jobs") {
			return []byte(running + "\n"), nil, nil
		}
		return nil, nil, nil
	}}
	d.execCmd = cmds.run
	d.tierSweepSeams.pollInterval = time.Hour
	d.tierSweepSeams.budgetTimer = runnerBudgetSpent

	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()
	if d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
		t.Fatalf("the cycle completed; the container stage was held by another run's Job, so it must defer. log:\n%s", logs.String())
	}

	if n := len(runnerCalls(cmds.ran, "apply")) + len(runnerCalls(cmds.ran, "create")); n != 0 {
		t.Errorf("%d objects applied or created while another run's Job runs: %v", n, cmds.ran)
	}
	if len(bd.created) != 0 {
		t.Errorf("%d beads filed for a deferred cycle, want none", len(bd.created))
	}
	for _, tier := range []string{"integration", "race"} {
		if strings.Contains(logs.String(), tier+" "+tierSweepRed) {
			t.Errorf("the cycle recorded %s RED for a Job it did not run: %q", tier, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "sweep job "+running+" is still running after "+tierSweepIntegrationBudget.String()) {
		t.Errorf("log does not name the running Job and the stage budget:\n%s", logs.String())
	}
}

// TestTierSweepRunningJobIsWaitedOnThenLaunched: while another run's Job is
// listed the launch is held, nothing is applied, and once the listing clears
// within the budget the run goes ahead.
func TestTierSweepRunningJobIsWaitedOnThenLaunched(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	running := otherRunJob(g)
	// The first listing shows the running Job; every later one shows none, as
	// the Job's end would. Other commands are answered by the healthy cluster.
	listings := 0
	g.cmds.answer = func(argv []string) ([]byte, []byte, error) {
		if slices.Contains(argv, "get") && slices.Contains(argv, "jobs") {
			listings++
			if listings == 1 {
				return []byte(running + "\n"), nil, nil
			}
			return nil, nil, nil
		}
		return g.answer(argv)
	}
	boundWaits(g, 5)

	stage := g.cycle()

	if stage.reason != "" || !stage.ran {
		t.Fatalf("stage = %+v, want the run launched after the wait and its result folded", stage)
	}
	if listings != 2 {
		t.Errorf("job listings = %d, want the launch asked again once the Job had ended", listings)
	}
	if !g.applied() {
		t.Errorf("the run was not launched after the Job ended: %v", g.cmds.ran)
	}
	if !strings.Contains(stage.output, "integration GREEN passed=66") {
		t.Errorf("stage output = %q, want the result folded", stage.output)
	}
}

// TestTierSweepSameRunJobIsAdopted: a Job already running this run (same sha and
// same tiers) is adopted: the stage waits on it and folds its result, and nothing
// is created or applied to start a second run.
func TestTierSweepSameRunJobIsAdopted(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	g.cluster.jobs = sweeprunner.JobName(g.spec) + "\n"
	// The launch record is what a restarted daemon adopts the run from, so it is
	// read at the first status read, while the run is still going.
	var recordAtStatus *tierSweepRunnerRecord
	g.cmds.answer = func(argv []string) ([]byte, []byte, error) {
		if recordAtStatus == nil && slices.Contains(argv, "get") && slices.Contains(argv, "job") {
			if rec, err := tierSweepReadRunnerRecord(g.spec.OutDir); err == nil {
				recordAtStatus = &rec
			}
		}
		return g.answer(argv)
	}
	boundWaits(g, 5)

	stage := g.cycle()

	if !stage.ran || stage.reason != "" {
		t.Fatalf("stage = %+v, want the adopted run's result folded", stage)
	}
	if !strings.Contains(stage.output, "integration GREEN passed=66") {
		t.Errorf("stage output = %q, want the adopted run's result", stage.output)
	}
	if n := len(runnerCalls(g.cmds.ran, "create")); n != 0 {
		t.Errorf("%d creates ran for an adopted run, want none: %v", n, g.cmds.ran)
	}
	for _, cmd := range g.cmds.ran {
		if strings.Contains(cmd, sweeprunner.RepoMount) {
			t.Errorf("the repo claim was filled for an adopted run: %q", cmd)
		}
	}
	if recordAtStatus == nil || recordAtStatus.RunID != g.spec.RunID {
		t.Errorf("launch record while the adopted run went on = %+v, want this run's record", recordAtStatus)
	}
	if !strings.Contains(g.logs.String(), "adopting run "+g.spec.RunID) {
		t.Errorf("log = %q, want the adoption named", g.logs.String())
	}
}

// TestTierSweepSameRunRecordPastBudgetIsReplaced: a launch record for this run
// that is older than the stage budget is a run that died with its daemon. The
// stage takes its objects down and starts its own run; the Job still listed is
// not a reason to defer or adopt it.
func TestTierSweepSameRunRecordPastBudgetIsReplaced(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	g := newGuardFixture(t, now)
	if err := os.MkdirAll(g.spec.OutDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tierSweepWriteRunnerRecord(g.spec.OutDir, tierSweepRunnerRecord{RunID: g.spec.RunID, Started: now.Add(-time.Hour)}); err != nil {
		t.Fatalf("writing the stale launch record: %v", err)
	}
	g.cluster.jobs = sweeprunner.JobName(g.spec) + "\n"
	// The teardown deletes the Job, so the listing that follows no longer shows it.
	var recordAtStatus *tierSweepRunnerRecord
	g.cmds.answer = func(argv []string) ([]byte, []byte, error) {
		if slices.Contains(argv, "delete") && slices.Contains(argv, "job") {
			g.cluster.jobs = ""
		}
		if recordAtStatus == nil && slices.Contains(argv, "get") && slices.Contains(argv, "job") {
			if rec, err := tierSweepReadRunnerRecord(g.spec.OutDir); err == nil {
				recordAtStatus = &rec
			}
		}
		return g.answer(argv)
	}
	boundWaits(g, 5)

	stage := g.cycle()

	if !stage.ran || stage.reason != "" {
		t.Fatalf("stage = %+v, want the run replaced and its result folded, not deferred", stage)
	}
	deletes := runnerCalls(g.cmds.ran, "delete", "job")
	applies := runnerCalls(g.cmds.ran, "apply")
	if len(deletes) == 0 {
		t.Fatalf("the stale run's Job was not taken down: %v", g.cmds.ran)
	}
	if len(applies) == 0 || applies[0] < deletes[0] {
		t.Errorf("the replacement applied before the stale run was taken down: %v", g.cmds.ran)
	}
	if recordAtStatus == nil || recordAtStatus.RunID != g.spec.RunID || !recordAtStatus.Started.Equal(now) {
		t.Errorf("launch record while the replacement ran = %+v, want this run started now", recordAtStatus)
	}
}
