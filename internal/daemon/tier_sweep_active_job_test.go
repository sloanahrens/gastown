package daemon

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// otherRunJob is the name of a sweep Job the renderer makes for the same rig at
// another sha: the Job a previous daemon's run left running, which holds the
// repo claim this run would rewrite (gt-pihhr4).
func otherRunJob(g *guardFixture) string {
	return otherRunJobAt(g, "fedcba9876543210fedcba9876543210fedcba98")
}

// otherRunJobAt is the name of the Job a run at sha makes for the rig's tiers.
func otherRunJobAt(g *guardFixture, sha string) string {
	other := g.spec
	other.SHA = sha
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
	d.notifier = notifyfake.New()
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

// TestTierSweepBusyJobPastBudgetEscalatesOnce: a foreign Job that outlasts the
// stage budget raises one escalation naming the rig and the Job. The next cycle,
// which meets the same Job again, defers and raises nothing more.
func TestTierSweepBusyJobPastBudgetEscalatesOnce(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	running := otherRunJob(g)
	g.cluster.jobs = running + "\n"
	g.d.tierSweepSeams.pollInterval = time.Hour
	g.d.tierSweepSeams.budgetTimer = runnerBudgetSpent

	for range 2 {
		if stage := g.cycle(); stage.ran {
			t.Fatalf("stage = %+v, want the cycle deferred behind %q", stage, running)
		}
	}

	got := g.notes.Escalations()
	if len(got) != 1 {
		t.Fatalf("escalations = %+v, want exactly one for the Job that outlasted the budget", got)
	}
	e := got[0].Escalation
	if e.Fingerprint != tierSweepBusyAlertKey(g.spec.Rig) {
		t.Errorf("escalation key = %q, want %q", e.Fingerprint, tierSweepBusyAlertKey(g.spec.Rig))
	}
	if !strings.Contains(e.Reason, "gastown") || !strings.Contains(e.Reason, running) {
		t.Errorf("escalation reason = %q, want the rig and the Job named", e.Reason)
	}
}

// TestTierSweepBusyJobEscalationClearsWhenJobEnds: the escalation raised for a
// foreign Job is closed once the Job has ended and the cycle launches its run.
func TestTierSweepBusyJobEscalationClearsWhenJobEnds(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	running := otherRunJob(g)
	g.cluster.jobs = running + "\n"
	g.d.tierSweepSeams.pollInterval = time.Hour
	g.d.tierSweepSeams.budgetTimer = runnerBudgetSpent
	if stage := g.cycle(); stage.ran {
		t.Fatalf("stage = %+v, want the cycle deferred behind %q", stage, running)
	}
	if n := len(g.notes.Escalations()); n != 1 {
		t.Fatalf("escalations before the Job ends = %d, want 1", n)
	}

	g.cluster.jobs = ""
	boundWaits(g, 5)
	stage := g.cycle()

	if stage.reason != "" || !stage.ran {
		t.Fatalf("stage = %+v, want the run launched once the Job ended", stage)
	}
	clears := g.notes.Clears()
	if len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, tierSweepBusyAlertKey(g.spec.Rig)) {
		t.Errorf("clears = %+v, want the escalation for %q closed", clears, running)
	}
}

// TestTierSweepBusyClearFailureIsLogged: a clear the notifier refuses is logged
// with the Job named, and the launch that followed still goes ahead.
func TestTierSweepBusyClearFailureIsLogged(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	running := otherRunJob(g)
	g.cluster.jobs = running + "\n"
	g.d.tierSweepSeams.pollInterval = time.Hour
	g.d.tierSweepSeams.budgetTimer = runnerBudgetSpent
	if stage := g.cycle(); stage.ran {
		t.Fatalf("stage = %+v, want the cycle deferred behind %q", stage, running)
	}

	g.notes.Fail(notifyfake.KindClear, errors.New("the escalation store refused the clear"))
	g.cluster.jobs = ""
	boundWaits(g, 5)
	stage := g.cycle()

	if stage.reason != "" || !stage.ran {
		t.Fatalf("stage = %+v, want the run launched even though the clear was refused", stage)
	}
	if !strings.Contains(g.logs.String(), "the escalation for sweep job "+running+" was not cleared") {
		t.Errorf("log = %q, want the refused clear named with its Job", g.logs.String())
	}
}

// TestTierSweepBusyJobReplacedRaisesItsOwnEscalation: when a second foreign Job
// holds the rig after the first, the first Job's escalation is closed and the
// second raises its own, naming it.
func TestTierSweepBusyJobReplacedRaisesItsOwnEscalation(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	first := otherRunJob(g)
	g.cluster.jobs = first + "\n"
	g.d.tierSweepSeams.pollInterval = time.Hour
	g.d.tierSweepSeams.budgetTimer = runnerBudgetSpent
	if stage := g.cycle(); stage.ran {
		t.Fatalf("stage = %+v, want the cycle deferred behind %q", stage, first)
	}

	second := otherRunJobAt(g, "89abcdef0123456789abcdef0123456789abcdef")
	g.cluster.jobs = second + "\n"
	if stage := g.cycle(); stage.ran {
		t.Fatalf("stage = %+v, want the cycle deferred behind %q", stage, second)
	}

	got := g.notes.Escalations()
	if len(got) != 2 || got[1].Escalation.Fingerprint != tierSweepBusyAlertKey(g.spec.Rig) {
		t.Fatalf("escalations = %+v, want one for %q after the first", got, second)
	}
	clears := g.notes.Clears()
	if len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, tierSweepBusyAlertKey(g.spec.Rig)) {
		t.Errorf("clears = %+v, want the first Job's escalation closed", clears)
	}
}

// TestTierSweepBusyWaitStartsAtStageStart: the wait for a foreign Job is the
// stage budget left after the launch's own reads, not a fresh budget started at
// the first launch. The first listing takes 50s of the 1m stage budget, so the
// cycle waits 10s for the Job.
func TestTierSweepBusyWaitStartsAtStageStart(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	running := otherRunJob(g)
	g.cluster.jobs = running + "\n"
	clk := g.clock(t)
	listed := false
	g.cmds.answer = func(argv []string) ([]byte, []byte, error) {
		if !listed && slices.Contains(argv, "get") && slices.Contains(argv, "jobs") {
			listed = true
			clk.Advance(50 * time.Second)
		}
		return g.answer(argv)
	}
	var waits []time.Duration
	g.d.tierSweepSeams.pollInterval = time.Hour
	g.d.tierSweepSeams.budgetTimer = func(wait time.Duration) (<-chan time.Time, func()) {
		waits = append(waits, wait)
		return runnerBudgetSpent(wait)
	}

	stage := g.cycle()

	if stage.ran {
		t.Fatalf("stage = %+v, want the cycle deferred behind %q", stage, running)
	}
	if len(waits) == 0 {
		t.Fatal("no wait was started for the foreign Job")
	}
	if want := 10 * time.Second; waits[0] != want {
		t.Errorf("wait for the foreign Job = %v, want %v: the stage budget counts from stage start", waits[0], want)
	}
}

// TestTierSweepBusyEscalationDeliveryFailureIsRetried: an escalation the
// notifier refuses leaves no record and is logged with the Job named, so the
// next cycle that meets the same Job raises it again; once delivered, the cycle
// after that raises nothing more (gt-pihhr4).
func TestTierSweepBusyEscalationDeliveryFailureIsRetried(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	running := otherRunJob(g)
	g.cluster.jobs = running + "\n"
	g.d.tierSweepSeams.pollInterval = time.Hour
	g.d.tierSweepSeams.budgetTimer = runnerBudgetSpent

	g.notes.Fail(notifyfake.KindEscalate, errors.New("the escalation store is down"))
	// The refused delivery is retried through the daemon's clock (1 s, 2 s), so
	// the cycle runs on a driven clock.
	var stage tierSweepStageResult
	runOnClock(t, g.d.clock.(*clockwork.FakeClock), time.Second, func() { stage = g.cycle() })
	if stage.ran {
		t.Fatalf("stage = %+v, want the cycle deferred behind %q", stage, running)
	}
	if !strings.Contains(g.logs.String(), "the escalation for sweep job "+running+" was not delivered") {
		t.Fatalf("log = %q, want the refused delivery named with its Job", g.logs.String())
	}
	if _, open := g.d.tierSweepBusy.openFor(g.spec.Rig); open {
		t.Fatalf("a refused escalation left a record open for %s; the next cycle would never retry it", g.spec.Rig)
	}

	g.notes.Fail(notifyfake.KindEscalate, nil)
	for range 2 {
		if stage := g.cycle(); stage.ran {
			t.Fatalf("stage = %+v, want the cycle deferred behind %q", stage, running)
		}
	}
	// The recorder keeps the refused attempts (the retries of the first cycle)
	// beside the delivered one; what matters is one delivery for the rig.
	delivered := 0
	for _, c := range g.notes.Escalations() {
		if c.Err == nil && c.Escalation.Fingerprint == tierSweepBusyAlertKey(g.spec.Rig) {
			delivered++
		}
	}
	if delivered != 1 {
		t.Errorf("delivered escalations after the store came back = %d (%+v), want exactly one for the rig", delivered, g.notes.Escalations())
	}
}

// TestTierSweepBusyEscalationLeftByAPreviousDaemonIsClearedOnce: a daemon that
// starts with no record of an open escalation cannot know whether its
// predecessor left one for the rig, so the first launch since start clears the
// rig's key once; the launch after it sends no further clear (gt-pihhr4).
func TestTierSweepBusyEscalationLeftByAPreviousDaemonIsClearedOnce(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	g.cluster.jobs = ""
	boundWaits(g, 5)

	for range 2 {
		if stage := g.cycle(); stage.reason != "" || !stage.ran {
			t.Fatalf("stage = %+v, want the run launched with no foreign Job", stage)
		}
		// The next cycle must launch again rather than adopt this run's result.
		if err := os.RemoveAll(g.spec.OutDir); err != nil {
			t.Fatal(err)
		}
	}

	clears := g.notes.Clears()
	if len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, tierSweepBusyAlertKey(g.spec.Rig)) {
		t.Errorf("clears = %+v, want exactly one for the rig's key on the first launch since start", clears)
	}
}

// TestTierSweepBusyFirstClearFailureIsOwedAgain: the first clear since start that
// the notifier refuses is logged and owed again, so the next launch sends it.
func TestTierSweepBusyFirstClearFailureIsOwedAgain(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	g.cluster.jobs = ""
	boundWaits(g, 5)
	g.notes.Fail(notifyfake.KindClear, errors.New("the escalation store refused the clear"))
	if stage := g.cycle(); !stage.ran {
		t.Fatalf("stage = %+v, want the run launched even though the clear was refused", stage)
	}
	if !strings.Contains(g.logs.String(), "a previous daemon may have left") {
		t.Fatalf("log = %q, want the refused first clear named", g.logs.String())
	}
	g.notes.Fail(notifyfake.KindClear, nil)
	// The next cycle must launch again rather than adopt the first run's result.
	if err := os.RemoveAll(g.spec.OutDir); err != nil {
		t.Fatal(err)
	}
	if stage := g.cycle(); !stage.ran {
		t.Fatalf("second stage = %+v, want the run launched", stage)
	}
	delivered := 0
	for _, c := range g.notes.Clears() {
		if c.Err == nil && slices.Contains(c.Fingerprints, tierSweepBusyAlertKey(g.spec.Rig)) {
			delivered++
		}
	}
	if delivered != 1 {
		t.Errorf("delivered clears = %d (%+v), want the retry after the refused one", delivered, g.notes.Clears())
	}
}
