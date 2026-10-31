package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// runnerTestSHA is a tip the runner stages can put in a Spec: 40 lower-case hex
// digits, the one shape sweeprunner.Spec accepts.
const runnerTestSHA = "0123456789abcdef0123456789abcdef01234567"

// runnerTestConfig is a runner block every field of which a backend can render.
func runnerTestConfig(t *testing.T) *config.TierSweepRunnerConfig {
	t.Helper()
	return &config.TierSweepRunnerConfig{
		Backend:    config.TierSweepRunnerDocker,
		Image:      "gt-polecat:test",
		CPUs:       8,
		Memory:     24,
		Jobs:       4,
		OutputRoot: filepath.Join(t.TempDir(), "out"),
		Context:    "colima-gt-sbx",
		Dind:       true,
	}
}

// runnerTestResult is the result.json a fake launch writes: the integration
// tier GREEN and the race tier RED, so one run shows both verdicts reaching the
// record.
func runnerTestResult(spec sweeprunner.Spec, now time.Time) sweeprunner.Result {
	return sweeprunner.Result{
		Rig:      spec.Rig,
		SHA:      spec.SHA,
		Started:  now.Add(-10 * time.Minute),
		Finished: now,
		Runner:   sweeprunner.Runner{Backend: config.TierSweepRunnerDocker, Image: spec.Image, CPUs: spec.CPUs},
		Tiers: []sweeprunner.Tier{
			{Tier: "integration", Summary: "tier-sweep: integration GREEN passed=66 failed=0 skipped=2 (logs /out/logs) in 5m2s", Command: "go test -count=1 ./..."},
			{Tier: "race", Summary: "tier-sweep: race RED passed=3 failed=1 skipped=0 failed: ./internal/cmd (logs /out/logs) in 2m1s"},
		},
	}
}

// runnerCycleDaemon is a sweep daemon whose runner block is set and whose shell
// stage is answered by the test, while the container stage keeps the production
// path: the runner branch is what the tests below exercise.
func runnerCycleDaemon(t *testing.T, now time.Time, cfg *config.TierSweepRunnerConfig, shell string) (*Daemon, *tierSweepFakeBeads) {
	t.Helper()
	d, _, bd, _ := newTierSweepDaemon(t, now, "gastown")
	d.patrolConfig.Patrols.TierSweep.Runner = cfg
	// The run's teardown goes through the daemon's command seam; no test here
	// starts a process.
	d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) { return nil, nil, nil }
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return runnerTestSHA, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.tierSweepSeams.run = func(ctx context.Context, dir string, st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return d.tierSweepRunStage(ctx, dir, st)
		}
		return tierSweepStageResult{output: shell, exitCode: 0, ran: true}
	}
	return d, bd
}

// With a runner configured, the container stage leaves the host: the daemon
// renders a Spec, launches it, reads the result.json the run wrote, and records
// the same verdicts, counts and logs a host stage's summary lines give it. The
// run starts its own engine, so the stage takes no container-gate slot
// (gt-z4ppb).
func TestTierSweepRunnerStageLaunchesAndReadsTheResult(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	const shellSummary = "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n"
	cfg := runnerTestConfig(t)
	d, bd := runnerCycleDaemon(t, now, cfg, shellSummary)

	type launch struct {
		st   tierSweepStage
		spec sweeprunner.Spec
	}
	var launches []launch
	d.tierSweepSeams.launch = func(_ context.Context, st tierSweepStage, spec sweeprunner.Spec) tierSweepLaunch {
		launches = append(launches, launch{st: st, spec: spec})
		if err := sweeprunner.Write(spec.OutDir, runnerTestResult(spec, now)); err != nil {
			t.Fatalf("the fake launch writing a result: %v", err)
		}
		return tierSweepLaunch{exitCode: 0}
	}

	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()
	if !d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
		t.Fatal("the cycle deferred; both stages ran and reported")
	}

	if len(launches) != 1 {
		t.Fatalf("launches = %d, want 1: only the container stage leaves the host", len(launches))
	}
	got := launches[0]
	if got.st.slot {
		t.Error("the launched stage still holds the container-gate slot; the runner's own engine means it takes none")
	}
	if want := []string{"integration", "race"}; !slices.Equal(got.st.tiers, want) {
		t.Errorf("launched tiers = %q, want %q", got.st.tiers, want)
	}
	if got.spec.Rig != "gastown" || got.spec.SHA != runnerTestSHA {
		t.Errorf("spec = %+v, want the rig and the tip the cycle checked out", got.spec)
	}
	if got.spec.Image != cfg.Image || got.spec.CPUs != cfg.CPUs || got.spec.MemoryGB != cfg.Memory ||
		got.spec.Jobs != cfg.Jobs || got.spec.Context != cfg.Context || got.spec.Dind != cfg.Dind {
		t.Errorf("spec = %+v, want the runner block's engine and limits", got.spec)
	}
	if !strings.HasPrefix(got.spec.OutDir, cfg.OutputRoot) {
		t.Errorf("spec.OutDir = %q, want it under the configured output root %q", got.spec.OutDir, cfg.OutputRoot)
	}
	if got.spec.RunID == "" || !strings.HasSuffix(got.spec.OutDir, got.spec.RunID) {
		t.Errorf("run id = %q, out dir = %q; want the run's own directory named after it", got.spec.RunID, got.spec.OutDir)
	}
	if want := filepath.Join(d.config.TownRoot, "gastown", ".repo.git"); got.spec.RepoDir != want {
		t.Errorf("spec.RepoDir = %q, want the rig's bare repository %q", got.spec.RepoDir, want)
	}

	st := tierSweepTestState(t, d, "gastown")
	integration := st.Tiers["integration"]
	if integration.Verdict != tierSweepGreen || integration.Passed != 66 || integration.Failed != 0 {
		t.Errorf("integration = %+v, want GREEN passed=66 from the result's own summary line", integration)
	}
	race := st.Tiers["race"]
	if race.Verdict != tierSweepRed || race.Failed != 1 || !slices.Contains(race.FailedNames, "./internal/cmd") {
		t.Errorf("race = %+v, want RED naming ./internal/cmd from the result's own summary line", race)
	}
	if race.LogDir != "/out/logs" {
		t.Errorf("race log dir = %q, want the unit log directory the result names", race.LogDir)
	}
	if !strings.HasSuffix(race.Log, filepath.Join("logs", "gastown", "integration.log")) {
		t.Errorf("race log = %q, want the stage log the daemon wrote", race.Log)
	}
	if out, err := os.ReadFile(race.Log); err != nil {
		t.Errorf("reading the stage log: %v", err)
	} else if !strings.Contains(string(out), "tier-sweep: race RED") {
		t.Errorf("stage log does not hold the result's summary line:\n%s", out)
	}
	if race.SlotWaitSeconds != 0 {
		t.Errorf("slot wait = %ds, want none: the runner holds no container-gate slot", race.SlotWaitSeconds)
	}
	if st.Tiers["shell"].Verdict != tierSweepGreen {
		t.Errorf("shell = %+v, want the host stage's verdict", st.Tiers["shell"])
	}
	if st.LastGreenSHA != "" {
		t.Errorf("LastGreenSHA = %q, want it held back by the RED race tier", st.LastGreenSHA)
	}

	var raceBeads, integrationBeads int
	for _, c := range bd.created {
		switch {
		case strings.HasPrefix(c.Title, tierSweepBeadPrefix("gastown", "race")):
			raceBeads++
		case strings.HasPrefix(c.Title, tierSweepBeadPrefix("gastown", "integration")):
			integrationBeads++
		}
	}
	if raceBeads != 1 || integrationBeads != 0 {
		t.Errorf("beads = %d race, %d integration; want one for the RED unit and none for the GREEN tier", raceBeads, integrationBeads)
	}
}

// A stage whose own budget passes with no result judged nothing: it is RED with
// the runner's own reason, never GREEN, and the run it waited on is taken down
// so its containers, network and volume are not left behind (gt-z4ppb).
func TestTierSweepRunnerBudgetWithNoResultTearsDown(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _ := runnerCycleDaemon(t, now, runnerTestConfig(t), "")
	var stops int
	d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
		return tierSweepLaunch{exitCode: 0, stop: func() { stops++ }}
	}
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: 25 * time.Millisecond}

	res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
	if res.ran != true || res.reason != tierSweepRunnerNoResult {
		t.Fatalf("stage result = %+v, want it run and RED with reason %q", res, tierSweepRunnerNoResult)
	}
	if !res.timedOut {
		t.Error("the stage did not report its own budget as the end of the wait; the cycle would defer instead of taking the RED")
	}
	if res.exitCode == 0 {
		t.Error("exit code 0 with no result; a tier with no verdict line must never read as GREEN")
	}
	if stops != 1 {
		t.Errorf("the run's teardown ran %d times, want once: the budget is a verdict about the run", stops)
	}
}

// A cycle that ends the wait takes no verdict: the run keeps going in its
// containers, so the stage reports that it did not run and the cycle defers it.
// Its teardown must not run, or the restart the cycle is preparing for would cut
// the very run that is supposed to survive it (gt-z4ppb).
func TestTierSweepRunnerCycleCancelLeavesTheRunAlive(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _ := runnerCycleDaemon(t, now, runnerTestConfig(t), "")
	var launches, stops, commands int
	d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) {
		commands++
		return nil, nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.tierSweepSeams.launch = func(_ context.Context, _ tierSweepStage, _ sweeprunner.Spec) tierSweepLaunch {
		launches++
		// The daemon begins its shutdown while the run is going.
		cancel()
		return tierSweepLaunch{exitCode: 0, stop: func() { stops++ }}
	}
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Hour}

	res := d.tierSweepRunStage(ctx, t.TempDir(), st)
	if res.ran {
		t.Errorf("stage result = %+v, want it not run: a canceled cycle judged nothing", res)
	}
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("stage err = %v, want the cycle's own cancellation", res.err)
	}
	if launches != 1 {
		t.Fatalf("launches = %d, want 1", launches)
	}
	if stops != 0 {
		t.Error("the stage tore the run down on a cycle cancel; the containers are what keeps the run going")
	}
	if commands != 0 {
		t.Errorf("commands ran %d times, want none: the run's teardown must not run on a cycle cancel", commands)
	}
}

// The run survives the cycle, so the next cycle must adopt it: the same rig, sha
// and tiers name the same run directory, a record there says it is going, and a
// result written while no cycle was watching folds as a host stage's verdicts
// would. Starting a second engine beside it would run the suite twice on one
// host (gt-z4ppb).
func TestTierSweepRunnerNextCycleAdoptsTheRunAndReadsItsResult(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _ := runnerCycleDaemon(t, now, runnerTestConfig(t), "")
	var launches, fakeStops int
	var teardowns []string
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		teardowns = append(teardowns, strings.Join(cmd.Args, " "))
		return nil, nil, nil
	}
	var launched sweeprunner.Spec
	ctx, cancel := context.WithCancel(context.Background())
	d.tierSweepSeams.launch = func(_ context.Context, _ tierSweepStage, spec sweeprunner.Spec) tierSweepLaunch {
		launches++
		launched = spec
		cancel()
		return tierSweepLaunch{exitCode: 0, stop: func() { fakeStops++ }}
	}
	// A short budget keeps the test quick if the run is not adopted: the wait
	// it bounds would otherwise be a minute of nothing.
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: 50 * time.Millisecond}

	if first := d.tierSweepRunStage(ctx, t.TempDir(), st); !errors.Is(first.err, context.Canceled) {
		t.Fatalf("first cycle = %+v, want it canceled while the run went", first)
	}
	// The run finishes while no cycle is watching, writing into the directory
	// every cycle for this sha names.
	if err := sweeprunner.Write(launched.OutDir, runnerTestResult(launched, now)); err != nil {
		t.Fatalf("writing the run's result: %v", err)
	}

	second := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
	if launches != 1 {
		t.Fatalf("launches = %d, want 1: the second cycle adopted the run instead of starting a second engine", launches)
	}
	if fakeStops != 0 {
		t.Errorf("the adopted run's launcher teardown ran %d times; the second cycle launched nothing to tear down", fakeStops)
	}
	if !second.ran || second.exitCode != tierSweepRedExit {
		t.Fatalf("second cycle = %+v, want it run and RED from the race tier's summary", second)
	}
	if !strings.Contains(second.output, "tier-sweep: integration GREEN") || !strings.Contains(second.output, "tier-sweep: race RED") {
		t.Errorf("folded output does not hold the result's summary lines:\n%s", second.output)
	}
	// The run is taken down once, after its result was read.
	if len(teardowns) != 4 {
		t.Errorf("teardown steps = %d, want the plan's four: %q", len(teardowns), teardowns)
	}
	for _, argv := range teardowns {
		if strings.Contains(argv, "run -d") {
			t.Errorf("a start step (%q) ran in the adopting cycle", argv)
		}
	}
	if _, err := os.Stat(filepath.Join(launched.OutDir, tierSweepRunnerRecordName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("launch record still present after the run was taken down (stat err = %v)", err)
	}
}

// A result that arrives torn judged nothing: the integration and race tiers are
// RED with the runner's own reason, never GREEN, and the teardown still runs so
// the containers are not left behind (gt-z4ppb).
func TestTierSweepRunnerMissingResultIsRed(t *testing.T) {
	t.Parallel()

	t.Run("a torn result", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, bd := runnerCycleDaemon(t, now, runnerTestConfig(t), "tier-sweep: shell GREEN passed=3 failed=0 skipped=0\n")
		d.tierSweepSeams.launch = func(_ context.Context, _ tierSweepStage, spec sweeprunner.Spec) tierSweepLaunch {
			if err := os.WriteFile(filepath.Join(spec.OutDir, sweeprunner.ResultFileName), []byte(`{"version":1,"rig":"gastown","sha":"`), 0o600); err != nil {
				t.Fatalf("the fake writing a torn result: %v", err)
			}
			return tierSweepLaunch{exitCode: 0}
		}
		cycle := d.startDogCycle("tier_sweep")
		defer cycle.close()
		if !d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
			t.Fatal("the cycle deferred; a torn result is a verdict, not an unfinished run")
		}

		st := tierSweepTestState(t, d, "gastown")
		for _, tier := range []string{"integration", "race"} {
			ts, ok := st.Tiers[tier]
			if !ok {
				t.Fatalf("tiers = %+v, want %s judged", st.Tiers, tier)
			}
			if ts.Verdict != tierSweepRed {
				t.Errorf("%s = %+v, want RED: the run's result could not be read", tier, ts)
			}
			if !slices.Contains(ts.FailedNames, tierSweepRunnerNoResult) {
				t.Errorf("%s failed names = %q, want %q", tier, ts.FailedNames, tierSweepRunnerNoResult)
			}
		}
		if st.LastGreenSHA != "" {
			t.Errorf("LastGreenSHA = %q, want it held back by a result the sweep never read", st.LastGreenSHA)
		}
		var filed int
		for _, c := range bd.created {
			if strings.Contains(c.Title, tierSweepRunnerNoResult) {
				filed++
			}
		}
		if filed != 2 {
			t.Errorf("beads naming the runner reason = %d, want one per unjudged tier (2)", filed)
		}
	})
}

// A runner the daemon cannot launch is refused, never replaced by the host
// path: a sweep run on the host would answer for a machine the operator took
// the tiers off (gt-z4ppb).
func TestTierSweepRunnerRefusesWhatItCannotLaunch(t *testing.T) {
	t.Parallel()

	t.Run("a spec no backend can render", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		cfg := runnerTestConfig(t)
		// Built in code rather than decoded, so the config's own range check
		// never ran: the spec's Validate is the authority the daemon renders
		// through.
		cfg.CPUs = 0
		d, _ := runnerCycleDaemon(t, now, cfg, "")
		st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Millisecond}

		res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
		if !res.ran || res.reason == "" || !strings.Contains(res.reason, "spec.cpus") {
			t.Fatalf("stage result = %+v, want it RED naming spec.cpus", res)
		}
	})

	t.Run("a backend the daemon cannot read a result from", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		cfg := runnerTestConfig(t)
		cfg.Backend = config.TierSweepRunnerKubernetes
		d, _ := runnerCycleDaemon(t, now, cfg, "")
		spec, ok, err := d.tierSweepRunnerSpec(tierSweepStage{tiers: []string{"integration"}, slot: true, rig: "gastown", tested: runnerTestSHA})
		if err != nil || !ok {
			t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
		}

		res := d.tierSweepLaunchRunner(context.Background(), tierSweepStage{}, spec)
		if res.err == nil || !strings.Contains(res.err.Error(), "cannot read a Job's result") {
			t.Fatalf("launch = %+v, want it refused for the Job's result it cannot read", res)
		}
	})

	t.Run("an unknown backend", func(t *testing.T) {
		t.Parallel()
		cfg := runnerTestConfig(t)
		if _, err := tierSweepRunnerBackend(cfg.Backend); err != nil {
			t.Fatalf("tierSweepRunnerBackend(%q) = %v", cfg.Backend, err)
		}
		_, err := tierSweepRunnerBackend("podman")
		if err == nil || !strings.Contains(err.Error(), config.TierSweepRunnerDocker) {
			t.Fatalf("tierSweepRunnerBackend(podman) = %v, want it refused naming the backends there are", err)
		}
	})
}

// A stage that asked for no tier judged nothing: it starts no run and folds no
// result, and an empty fold never reads as a clean sweep of nothing (gt-z4ppb).
func TestTierSweepRunnerNoTiersIsRed(t *testing.T) {
	t.Parallel()

	t.Run("the stage starts nothing", func(t *testing.T) {
		t.Parallel()
		d, _ := runnerCycleDaemon(t, atHour(time.Now(), 15), runnerTestConfig(t), "")
		launched := false
		d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
			launched = true
			return tierSweepLaunch{exitCode: 0}
		}
		st := tierSweepStage{slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Second}

		res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
		if res.reason != tierSweepRunnerNoResult || !strings.Contains(res.output, "no tiers asked for") {
			t.Fatalf("stage result = %+v, want RED with reason %q naming that no tier was asked for", res, tierSweepRunnerNoResult)
		}
		if launched {
			t.Error("a stage that asked for no tier launched a run")
		}
		if res.exitCode == 0 {
			t.Error("exit code 0 with no tier; the cycle would record a clean sweep of nothing")
		}
	})

	t.Run("the fold refuses one", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, _ := runnerCycleDaemon(t, now, runnerTestConfig(t), "")
		spec, ok, err := d.tierSweepRunnerSpec(tierSweepStage{tiers: []string{"integration"}, slot: true, rig: "gastown", tested: runnerTestSHA})
		if err != nil || !ok {
			t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
		}

		res := d.tierSweepRunnerStageResult(tierSweepStage{rig: "gastown", tested: runnerTestSHA}, spec, runnerTestResult(spec, now), tierSweepLaunch{exitCode: -1})
		if res.reason != tierSweepRunnerNoResult || !strings.Contains(res.output, "no tiers asked for") {
			t.Fatalf("folded stage result = %+v, want RED naming that no tier was asked for", res)
		}
		if res.exitCode == 0 {
			t.Error("exit code 0 for a fold of no tier; an empty fold must not read as GREEN")
		}
	})
}

// runnerFakeCmds answers the launcher's commands without starting a process. It
// records each command's argv and whether the context it was bound to was
// already canceled: exec calls Cancel when that context is done, so calling it
// reports the same thing.
type runnerFakeCmds struct {
	ran     []string
	ctxDone []bool
	answer  func(argv []string) (stdout, stderr []byte, err error)
}

func (f *runnerFakeCmds) run(cmd *exec.Cmd) ([]byte, []byte, error) {
	f.ran = append(f.ran, strings.Join(cmd.Args, " "))
	f.ctxDone = append(f.ctxDone, cmd.Cancel != nil && cmd.Cancel() != nil)
	if f.answer != nil {
		return f.answer(cmd.Args)
	}
	return nil, nil, nil
}

// runnerLauncher is a daemon with a runner block whose launcher runs no
// process: every step answers through cmds. It returns the Spec and stage the
// launcher is driven with.
func runnerLauncher(t *testing.T, cmds *runnerFakeCmds) (*Daemon, sweeprunner.Spec, tierSweepStage, *bytes.Buffer) {
	t.Helper()
	d, _, _, logs := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.patrolConfig.Patrols.TierSweep.Runner = runnerTestConfig(t)
	d.execCmd = cmds.run
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Hour}
	spec, ok, err := d.tierSweepRunnerSpec(st)
	if err != nil || !ok {
		t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
	}
	return d, spec, st, logs
}

// The production launcher, driven step by step through the daemon's command
// seam: a start that fails still hands back the teardown, a teardown that fails
// is logged rather than returned, and the teardown outlives the context the run
// was launched under (gt-z4ppb).
func TestTierSweepRunnerLauncher(t *testing.T) {
	t.Parallel()

	t.Run("a start step that fails still returns the teardown", func(t *testing.T) {
		t.Parallel()
		cmds := &runnerFakeCmds{answer: func(argv []string) ([]byte, []byte, error) {
			if slices.Contains(argv, "network") {
				return nil, []byte("Error: cannot create network\n"), errors.New("exit status 1")
			}
			return nil, nil, nil
		}}
		d, spec, st, _ := runnerLauncher(t, cmds)

		started := d.tierSweepLaunchRunner(context.Background(), st, spec)
		if started.err == nil || !strings.Contains(started.err.Error(), "did not start") {
			t.Fatalf("launch = %+v, want the failed start reported", started)
		}
		if started.stop == nil {
			t.Fatal("a failed start returned no teardown; a network or volume an earlier step made would be left behind")
		}
		if len(cmds.ran) != 1 {
			t.Fatalf("steps run = %q, want only the one that failed", cmds.ran)
		}
		started.stop()
		if len(cmds.ran) != 5 {
			t.Fatalf("steps run = %q, want the failed start and the plan's four teardown steps", cmds.ran)
		}
		for _, argv := range cmds.ran[1:] {
			if !strings.Contains(argv, "rm") {
				t.Errorf("step %q ran after the failed start; only the plan's teardown may", argv)
			}
		}
	})

	t.Run("a teardown step that fails is logged, not returned", func(t *testing.T) {
		t.Parallel()
		cmds := &runnerFakeCmds{answer: func(argv []string) ([]byte, []byte, error) {
			if slices.Contains(argv, "rm") {
				return nil, []byte("Error: No such container: gt-sweep-gastown-x\n"), errors.New("exit status 1")
			}
			return nil, nil, nil
		}}
		d, spec, st, logs := runnerLauncher(t, cmds)

		started := d.tierSweepLaunchRunner(context.Background(), st, spec)
		if started.err != nil || started.stop == nil {
			t.Fatalf("launch = %+v, want it started with a teardown", started)
		}
		started.stop()
		if !strings.Contains(logs.String(), "removing the run's objects") {
			t.Errorf("the failed teardown was not logged:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "No such container") {
			t.Errorf("the log does not hold the teardown's last lines:\n%s", logs.String())
		}
	})

	t.Run("a nil runner block is refused", func(t *testing.T) {
		t.Parallel()
		cmds := &runnerFakeCmds{}
		d, spec, st, _ := runnerLauncher(t, cmds)
		d.patrolConfig.Patrols.TierSweep.Runner = nil

		started := d.tierSweepLaunchRunner(context.Background(), st, spec)
		if started.err == nil || !strings.Contains(started.err.Error(), "no runner is configured") {
			t.Fatalf("launch = %+v, want it refused naming the missing runner block", started)
		}
		if started.stop != nil {
			t.Error("nothing was made, so there is nothing to tear down")
		}
		if len(cmds.ran) != 0 {
			t.Errorf("steps run = %q, want none: a runner that is not configured cannot launch", cmds.ran)
		}
	})

	t.Run("the teardown outlives the context it was launched under", func(t *testing.T) {
		t.Parallel()
		cmds := &runnerFakeCmds{}
		d, spec, st, _ := runnerLauncher(t, cmds)
		ctx, cancel := context.WithCancel(context.Background())

		started := d.tierSweepLaunchRunner(ctx, st, spec)
		if started.err != nil || started.stop == nil {
			t.Fatalf("launch = %+v, want it started with a teardown", started)
		}
		launched := len(cmds.ran)
		// The cycle ends the wait; the containers still have to come down.
		cancel()
		started.stop()
		if got := len(cmds.ran) - launched; got != 4 {
			t.Fatalf("teardown steps = %d, want the plan's four after the caller's context was canceled: %q", got, cmds.ran[launched:])
		}
		for i, argv := range cmds.ran[launched:] {
			if cmds.ctxDone[launched+i] {
				t.Errorf("teardown step %q ran on a canceled context", argv)
			}
		}
	})
}

// The two names the config declares are the names sweeprunner's backends
// answer to, so a rename in either package cannot leave the config naming an
// engine the daemon does not build (gt-z4ppb).
func TestTierSweepRunnerBackendNamesAgreeWithTheRunner(t *testing.T) {
	t.Parallel()
	for _, name := range []string{config.TierSweepRunnerDocker, config.TierSweepRunnerKubernetes} {
		if name != (sweeprunner.Docker{}).Name() && name != (sweeprunner.Kubernetes{}).Name() {
			t.Errorf("config names backend %q, which no sweeprunner backend answers to", name)
		}
	}
	if backend, err := tierSweepRunnerBackend(config.TierSweepRunnerDocker); err != nil || backend.Name() != config.TierSweepRunnerDocker {
		t.Errorf("tierSweepRunnerBackend(%q) = %v, %v; want the backend named %q", config.TierSweepRunnerDocker, backend, err, config.TierSweepRunnerDocker)
	}
}

// The launch record is what keeps a second cycle from starting a second engine
// beside a live run, so every way it can fail to be written or read has an
// answer: a record that cannot be read is an error, never "no run", and a stage
// that finds one stops instead of launching (gt-z4ppb).
func TestTierSweepRunnerLaunchRecord(t *testing.T) {
	t.Parallel()

	t.Run("a write names its run", func(t *testing.T) {
		t.Parallel()
		err := tierSweepWriteRunnerRecord(t.TempDir(), tierSweepRunnerRecord{Started: time.Now()})
		if err == nil || !strings.Contains(err.Error(), "names no run") {
			t.Fatalf("writing a record with no run id = %v, want it refused", err)
		}
	})

	t.Run("a write into a missing directory fails", func(t *testing.T) {
		t.Parallel()
		err := tierSweepWriteRunnerRecord(filepath.Join(t.TempDir(), "gone"), tierSweepRunnerRecord{RunID: "run", Started: time.Now()})
		if err == nil || !strings.Contains(err.Error(), "writing") {
			t.Fatalf("writing into a directory that is not there = %v, want the write error", err)
		}
	})

	t.Run("a torn record is an error, not a missing one", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, tierSweepRunnerRecordName), []byte(`{"run_id":`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := tierSweepReadRunnerRecord(dir)
		if err == nil || errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "reading") {
			t.Fatalf("reading a torn record = %v, want a read error that is not \"no record\"", err)
		}
	})

	t.Run("a record naming no run is refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, tierSweepRunnerRecordName), []byte(`{"run_id":""}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := tierSweepReadRunnerRecord(dir)
		if err == nil || !strings.Contains(err.Error(), "names no run") {
			t.Fatalf("reading a record that names no run = %v, want it refused", err)
		}
	})

	t.Run("a missing record is not an error", func(t *testing.T) {
		t.Parallel()
		if _, err := tierSweepReadRunnerRecord(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reading a missing record = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("the stage refuses a torn record instead of launching", func(t *testing.T) {
		t.Parallel()
		d, _ := runnerCycleDaemon(t, atHour(time.Now(), 15), runnerTestConfig(t), "")
		launched := false
		d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
			launched = true
			return tierSweepLaunch{exitCode: 0}
		}
		st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Second}
		spec, ok, err := d.tierSweepRunnerSpec(st)
		if err != nil || !ok {
			t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
		}
		if err := os.MkdirAll(spec.OutDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(spec.OutDir, tierSweepRunnerRecordName), []byte(`{`), 0o600); err != nil {
			t.Fatal(err)
		}

		res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
		if res.reason != tierSweepRunnerNoResult {
			t.Fatalf("stage result = %+v, want RED with reason %q", res, tierSweepRunnerNoResult)
		}
		if launched {
			t.Error("a stage that could not read the run's record launched a second engine beside it")
		}
	})
}
