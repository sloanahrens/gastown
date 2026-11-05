package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
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
	if want := filepath.Join(got.spec.OutDir, sweeprunner.LogsDirName); race.LogDir != want {
		t.Errorf("race log dir = %q, want the host run directory %q, not the engine's /out/logs", race.LogDir, want)
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

// TestTierSweepRunnerSpec_TakesAScriptSlotStage pins which stages the
// configured runner takes: a script slot stage leaves the host whatever command
// its tree runs, because cmd/sweep-run runs that script in the container. A
// slot stage with no runner block, and a stage that starts no container, stay on
// the host. Restoring the script exclusion turns the first row red (gt-rhwx1).
func TestTierSweepRunnerSpec_TakesAScriptSlotStage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		runner bool // whether the patrol config names a runner block
		stage  tierSweepStage
		wantOK bool
	}{
		{
			name:   "a script stage with a runner block is rendered",
			runner: true,
			stage:  tierSweepStage{tiers: []string{"integration", "race"}, slot: true, script: true, rig: "devops", tested: runnerTestSHA},
			wantOK: true,
		},
		{
			name:  "a script stage with no runner block stays on the host",
			stage: tierSweepStage{tiers: []string{"integration", "race"}, slot: true, script: true, rig: "devops", tested: runnerTestSHA},
		},
		{
			name:   "a stage that starts no container stays on the host",
			runner: true,
			stage:  tierSweepStage{tiers: []string{"shell"}, script: true, rig: "devops", tested: runnerTestSHA},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "devops")
			var cfg *config.TierSweepRunnerConfig
			if tc.runner {
				cfg = runnerTestConfig(t)
				d.patrolConfig.Patrols.TierSweep.Runner = cfg
			}

			spec, ok, err := d.tierSweepRunnerSpec(tc.stage)
			if err != nil {
				t.Fatalf("tierSweepRunnerSpec = %v, want no error", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if spec.Rig != tc.stage.rig || spec.SHA != runnerTestSHA || !slices.Equal(spec.Tiers, tc.stage.tiers) {
				t.Errorf("spec = %+v, want the stage's rig %s, sha %s and tiers %q", spec, tc.stage.rig, runnerTestSHA, tc.stage.tiers)
			}
			if spec.Image != cfg.Image {
				t.Errorf("spec.Image = %q, want the runner block's image %q", spec.Image, cfg.Image)
			}
		})
	}
}

// TestTierSweepShellStageGoesToTheRunnerWhenEnabled: runner.shell is the
// opt-in that sends the shell stage to the runner. With it set the stage is
// launched, and the result's shell summary and exit are recorded exactly as
// the integration and race results are; without it the stage runs on the host
// and the host runner's summary is what the record holds (gt-b0mwr).
func TestTierSweepShellStageGoesToTheRunnerWhenEnabled(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	const (
		hostShell   = "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /out/host-logs)\n"
		foldedShell = "tier-sweep: shell RED passed=2 failed=1 skipped=0 failed: scripts/a_test.sh (logs /out/logs) in 1m1s"
		otherTiers  = "tier-sweep: integration GREEN passed=5 failed=0 skipped=0 (logs /out/logs) in 1m\ntier-sweep: race GREEN passed=2 failed=0 skipped=0 (logs /out/logs) in 1m\n"
	)
	for _, tc := range []struct {
		name           string
		shell          bool
		wantOffHost    bool
		wantVerdict    string
		wantPassed     int
		wantFailedName string
	}{
		{name: "enabled", shell: true, wantOffHost: true, wantVerdict: tierSweepRed, wantPassed: 2, wantFailedName: "scripts/a_test.sh"},
		{name: "disabled", shell: false, wantOffHost: false, wantVerdict: tierSweepGreen, wantPassed: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := runnerTestConfig(t)
			cfg.Shell = tc.shell
			d, _ := runnerCycleDaemon(t, now, cfg, hostShell)
			var launched []sweeprunner.Spec
			d.tierSweepSeams.launch = func(_ context.Context, _ tierSweepStage, spec sweeprunner.Spec) tierSweepLaunch {
				launched = append(launched, spec)
				if err := sweeprunner.Write(spec.OutDir, sweeprunner.Result{
					Rig:      spec.Rig,
					SHA:      spec.SHA,
					Started:  now.Add(-time.Minute),
					Finished: now,
					Runner:   sweeprunner.Runner{Backend: config.TierSweepRunnerDocker, Image: spec.Image, CPUs: spec.CPUs},
					Tiers:    []sweeprunner.Tier{{Tier: "shell", Summary: foldedShell}},
				}); err != nil {
					t.Fatalf("the fake launch writing a result: %v", err)
				}
				return tierSweepLaunch{exitCode: 0}
			}
			// The integration stage is answered on the host so only the shell
			// stage can reach the launcher; the shell stage takes the production
			// dispatch when runner.shell sends it off the host.
			d.tierSweepSeams.run = func(ctx context.Context, dir string, st tierSweepStage) tierSweepStageResult {
				if !st.shell {
					return tierSweepStageResult{output: otherTiers, exitCode: 0, ran: true}
				}
				// Which branch the stage takes is the production dispatch's
				// call, not this test's expectation: only the host step's
				// execution is stubbed below, because a unit test starts no
				// process.
				if _, offHost, err := d.tierSweepRunnerSpec(st); err != nil || offHost {
					return d.tierSweepRunStage(ctx, dir, st)
				}
				return tierSweepStageResult{output: hostShell, exitCode: 0, ran: true}
			}

			stages := tierSweepStages("gastown")
			shellIdx := slices.IndexFunc(stages, func(st tierSweepStage) bool { return st.shell })
			if shellIdx < 0 {
				t.Fatal("tierSweepStages has no stage with shell set")
			}
			shellStage := stages[shellIdx]
			shellStage.tested = runnerTestSHA
			_, offHost, err := d.tierSweepRunnerSpec(shellStage)
			if err != nil {
				t.Fatalf("tierSweepRunnerSpec of the shell stage = %v", err)
			}
			if offHost != tc.wantOffHost {
				t.Fatalf("the shell stage off the host = %v, want %v with runner.shell %v", offHost, tc.wantOffHost, tc.shell)
			}

			cycle := d.startDogCycle("tier_sweep")
			defer cycle.close()
			if !d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
				t.Fatal("the cycle deferred; both stages ran and reported")
			}

			if tc.wantOffHost {
				if len(launched) != 1 {
					t.Fatalf("launches = %d, want 1: the shell stage leaves the host", len(launched))
				}
				if want := []string{"shell"}; !slices.Equal(launched[0].Tiers, want) {
					t.Errorf("launched tiers = %q, want %q", launched[0].Tiers, want)
				}
			} else if len(launched) != 0 {
				t.Fatalf("launches = %d, want none: runner.shell is off", len(launched))
			}

			shell := tierSweepTestState(t, d, "gastown").Tiers["shell"]
			if shell.Verdict != tc.wantVerdict || shell.Passed != tc.wantPassed {
				t.Errorf("shell = %+v, want %s passed=%d", shell, tc.wantVerdict, tc.wantPassed)
			}
			if tc.wantFailedName != "" && !slices.Contains(shell.FailedNames, tc.wantFailedName) {
				t.Errorf("shell failed names = %q, want %q from the result's summary", shell.FailedNames, tc.wantFailedName)
			}
		})
	}
}

// TestTierSweepRunnerStageLaunchesAndFoldsAScriptStage drives the stage
// dispatch a cycle calls: a script slot stage with a runner configured leaves
// the host, its launch is handed the script stage, and the run's result folds
// into the summary lines and RED exit the cycle records. The context is
// cancelled first so that, with the script exclusion restored, the stage falls
// to the host gate and that gate refuses its slot at once instead of waiting on
// the town's real one: the launch count is then what goes red (gt-rhwx1).
func TestTierSweepRunnerStageLaunchesAndFoldsAScriptStage(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _ := runnerCycleDaemon(t, now, runnerTestConfig(t), "")
	var (
		launches int
		launched tierSweepStage
	)
	d.tierSweepSeams.launch = func(_ context.Context, st tierSweepStage, spec sweeprunner.Spec) tierSweepLaunch {
		launches++
		launched = st
		if err := sweeprunner.Write(spec.OutDir, runnerTestResult(spec, now)); err != nil {
			t.Fatalf("the fake launch writing a result: %v", err)
		}
		return tierSweepLaunch{exitCode: 0}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, script: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}
	res := d.tierSweepRunStage(ctx, t.TempDir(), st)
	if launches != 1 {
		t.Fatalf("launches = %d, want 1: a script stage a runner is configured for leaves the host", launches)
	}
	if !launched.script || !slices.Equal(launched.tiers, st.tiers) {
		t.Errorf("launched stage = %+v, want the script flag and tiers %q", launched, st.tiers)
	}
	if !res.ran || res.exitCode != tierSweepRedExit {
		t.Fatalf("stage result = %+v, want it run and RED from the race tier's summary", res)
	}
	if !strings.Contains(res.output, "tier-sweep: integration GREEN") || !strings.Contains(res.output, "tier-sweep: race RED") {
		t.Errorf("folded output does not hold the result's summary lines:\n%s", res.output)
	}
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
	// stdin is what each command read from its input, nil for one that read none.
	stdin  [][]byte
	answer func(argv []string) (stdout, stderr []byte, err error)
}

func (f *runnerFakeCmds) run(cmd *exec.Cmd) ([]byte, []byte, error) {
	f.ran = append(f.ran, strings.Join(cmd.Args, " "))
	f.ctxDone = append(f.ctxDone, cmd.Cancel != nil && cmd.Cancel() != nil)
	var in []byte
	if cmd.Stdin != nil {
		in, _ = io.ReadAll(cmd.Stdin) // the launcher's input is a bytes.Reader, which never fails
	}
	f.stdin = append(f.stdin, in)
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

// runnerKubernetesDaemon is a daemon whose runner block names the Kubernetes
// backend, with the Spec of the stage it runs. It sets no command seam: the test
// does, because the answers depend on the Spec.
func runnerKubernetesDaemon(t *testing.T, now time.Time) (*Daemon, sweeprunner.Spec, tierSweepStage) {
	t.Helper()
	d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
	cfg := runnerTestConfig(t)
	cfg.Backend = config.TierSweepRunnerKubernetes
	d.patrolConfig.Patrols.TierSweep.Runner = cfg
	// The budget is short so a run whose result is never copied is judged RED in
	// about a second; the copy path reads its result without waiting on it.
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Second}
	spec, ok, err := d.tierSweepRunnerSpec(st)
	if err != nil || !ok {
		t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
	}
	return d, spec, st
}

// runnerCluster answers the kubectl commands one Kubernetes run makes. The Job
// prints status when it is asked: statusErr fails every read, and flakyStatus
// fails only the first that many. The sync guard's listing of the Jobs answers
// jobs, or fails when jobsErr is set. A copy writes result into the directory
// it names, fails the way kubectl does when the claim holds no result, or
// succeeds and writes nothing when skipWrite is set; failCopy fails the copy out
// of the output claim outright, and failSyncCopy the copy into the repo claim.
// noLogs fails the logs copy the way kubectl does when the run wrote none.
// failSyncCreate fails the sync pod's create the way a concurrent sync's does,
// with AlreadyExists. failApply is the 1-based apply that fails, 0 for none: 1
// is the repo claim, 2 the output claim, 3 the Job and 4 the reader pod.
// failWait fails the reader pod's readiness wait, not the sync pod's.
// priority answers the priority preflight's listing instead of the healthy one,
// so a cycle can be driven from a protected pod the sweep outranks to a healthy
// cluster and back.
// capacity answers the capacity preflight; empty is runnerRoomyCapacity, so a
// test about the launch itself is not declined for want of room.
type runnerCluster struct {
	t              *testing.T
	status         string
	statusErr      bool
	flakyStatus    int
	statusReads    int
	jobs           string
	jobsErr        bool
	capacity       string
	result         *sweeprunner.Result
	skipWrite      bool
	failCopy       bool
	noLogs         bool
	failSyncCopy   bool
	failSyncCreate bool
	failWait       bool
	failApply      int
	applies        int
	priority       []byte
}

// runnerPriorityListing is what the priority preflight's kubectl call answers:
// the sweep's own class, the control plane's, and the three control-plane pods
// the table records at their classes, so a run with nothing to shed passes.
const runnerPriorityListing = `{"kind":"List","items":[
{"kind":"PriorityClass","metadata":{"name":"gt-ci"},"value":100},
{"kind":"PriorityClass","metadata":{"name":"gt-ci-gate"},"value":200},
{"kind":"PriorityClass","metadata":{"name":"gt-control-plane"},"value":1000000},
{"kind":"Pod","metadata":{"name":"forgejo-57658978cf-5nzvk","namespace":"forgejo"},"spec":{"priority":1000000,"ownerReferences":[{"kind":"ReplicaSet","name":"forgejo-57658978cf"}]}},
{"kind":"Pod","metadata":{"name":"forgejo-runner-df77dd979-g25pq","namespace":"forgejo"},"spec":{"priority":200,"ownerReferences":[{"kind":"ReplicaSet","name":"forgejo-runner-df77dd979"}]}},
{"kind":"Pod","metadata":{"name":"forgejo-6884fd5d78-6mqpl","namespace":"forgejo-trial"},"spec":{"priority":1000000,"ownerReferences":[{"kind":"ReplicaSet","name":"forgejo-6884fd5d78"}]}}
]}`

// runnerRoomyCapacity is a node with room for the default runner block's Job
// (8 CPU, 24 GiB with dind) and the control plane's headroom.
const runnerRoomyCapacity = `{"items": [{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"allocatable": {"cpu": "16", "memory": "64Gi"}}}]}`

func (c *runnerCluster) answer(argv []string) ([]byte, []byte, error) {
	switch {
	case slices.Contains(argv, "get") && slices.Contains(argv, "pods,priorityclasses"):
		if c.priority != nil {
			return c.priority, nil, nil
		}
		return []byte(runnerPriorityListing), nil, nil
	case slices.Contains(argv, "get") && slices.Contains(argv, "jobs"):
		if c.jobsErr {
			return nil, []byte("Error from server (Forbidden): jobs.batch is forbidden\n"), errors.New("exit status 1")
		}
		return []byte(c.jobs), nil, nil
	case slices.Contains(argv, "get") && slices.Contains(argv, "nodes,pods"):
		if c.capacity != "" {
			return []byte(c.capacity), nil, nil
		}
		return []byte(runnerRoomyCapacity), nil, nil
	case slices.Contains(argv, "get"):
		c.statusReads++
		if c.statusErr || c.statusReads <= c.flakyStatus {
			return nil, []byte("Error from server (NotFound): jobs.batch not found\n"), errors.New("exit status 1")
		}
		return []byte(c.status), nil, nil
	case slices.Contains(argv, "create"):
		if c.failSyncCreate {
			return nil, []byte("Error from server (AlreadyExists): pods \"gt-sweep-sync-gastown\" already exists\n"), errors.New("exit status 1")
		}
	case slices.Contains(argv, "apply"):
		c.applies++
		if c.applies == c.failApply {
			return nil, []byte("error: the server refused the document\n"), errors.New("exit status 1")
		}
	case slices.Contains(argv, "wait"):
		if c.failWait && !slices.ContainsFunc(argv, func(arg string) bool { return strings.HasPrefix(arg, "pod/gt-sweep-sync-") }) {
			return nil, []byte("error: timed out waiting for the condition\n"), errors.New("exit status 1")
		}
	case slices.Contains(argv, "cp"):
		// The sync stage's copy lands in the repo claim; the reader's copies
		// bring the result and the per-unit logs home.
		if strings.HasSuffix(argv[len(argv)-1], sweeprunner.RepoMount) {
			if c.failSyncCopy {
				return nil, []byte("error: unable to copy into the sync pod\n"), errors.New("exit status 1")
			}
			return nil, nil, nil
		}
		logsSrc := ":" + sweeprunner.OutputDir + "/" + sweeprunner.LogsDirName
		if slices.ContainsFunc(argv, func(arg string) bool { return strings.HasSuffix(arg, logsSrc) }) {
			if c.noLogs {
				return nil, []byte("error: tar: /out/logs: Cannot stat: No such file or directory\n"), errors.New("exit status 1")
			}
			return nil, nil, nil
		}
		if c.failCopy {
			return nil, []byte("error: unable to copy from the pod\n"), errors.New("exit status 1")
		}
		if c.result == nil {
			return nil, []byte("error: cannot stat the result in the claim\n"), errors.New("exit status 1")
		}
		if !c.skipWrite {
			if err := sweeprunner.Write(filepath.Dir(argv[len(argv)-1]), *c.result); err != nil {
				c.t.Errorf("the fake copy writing the result: %v", err)
			}
		}
	}
	return nil, nil, nil
}

// runnerCalls returns the indexes of the commands whose argv holds every word.
func runnerCalls(ran []string, words ...string) []int {
	var at []int
	for i, cmd := range ran {
		fields := strings.Fields(cmd)
		all := true
		for _, w := range words {
			if !slices.Contains(fields, w) {
				all = false
				break
			}
		}
		if all {
			at = append(at, i)
		}
	}
	return at
}

// A Kubernetes run is launched as its plan renders it: the sync stage fills the
// repo claim, the output claim is applied before the Job, both as the rendered
// documents, and the stage folds the verdicts of the result the copy brought
// back (gt-2j3t7).
func TestTierSweepRunnerKubernetesLaunchesJob(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, spec, st := runnerKubernetesDaemon(t, now)
	result := runnerTestResult(spec, now)
	cmds := &runnerFakeCmds{answer: (&runnerCluster{t: t, status: "Complete", result: &result}).answer}
	d.execCmd = cmds.run

	stage := d.tierSweepRunRunner(context.Background(), st, spec)

	plan, err := sweeprunner.Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the spec: %v", err)
	}
	// The output claim's document, wherever in the plan it is applied.
	var outClaim []byte
	for _, step := range plan.Apply {
		var claim sweeprunner.Claim
		if json.Unmarshal(step.Stdin, &claim) == nil && claim.Metadata.Name == sweeprunner.OutClaimName(spec) {
			outClaim = step.Stdin
		}
	}
	if outClaim == nil {
		t.Fatalf("the rendered plan applies no output claim %q", sweeprunner.OutClaimName(spec))
	}
	claimAt, jobAt, syncAt := -1, -1, -1
	for i := range cmds.ran {
		switch {
		case claimAt < 0 && bytes.Equal(cmds.stdin[i], outClaim):
			claimAt = i
		case jobAt < 0 && bytes.Equal(cmds.stdin[i], plan.Job.JSON()):
			jobAt = i
		case syncAt < 0 && bytes.Equal(cmds.stdin[i], plan.Apply[1].Stdin):
			syncAt = i
		}
	}
	if claimAt < 0 {
		t.Fatalf("the output claim was never applied as its rendered document; commands: %q", cmds.ran)
	}
	if jobAt < 0 {
		t.Fatalf("the Job was never applied as its rendered document; commands: %q", cmds.ran)
	}
	if jobAt < claimAt {
		t.Errorf("the Job was applied at command %d, before its claim at %d: it would have nothing to write its result to", jobAt, claimAt)
	}
	if syncAt < 0 || syncAt > claimAt {
		t.Errorf("the sync pod was applied at command %d, want it before the output claim at %d: the Job would read a claim the sync stage never filled", syncAt, claimAt)
	}
	if stage.reason != "" {
		t.Fatalf("stage = %+v, want the copied result folded, not RED", stage)
	}
	for _, want := range []string{"integration GREEN passed=66", "race RED"} {
		if !strings.Contains(stage.output, want) {
			t.Errorf("stage output does not carry the copied verdict %q:\n%s", want, stage.output)
		}
	}
	if stage.exitCode != tierSweepRedExit {
		t.Errorf("exit code = %d, want the sweep's RED exit: the race tier the copied result names is RED", stage.exitCode)
	}
}

// TestRenderPlanSyncStepFailureIsRed drives the sync stage's copy through a
// fake kubectl that fails it: the launch stops at the failing step, so the Job
// is never applied, and the stage is RED carrying the step's stderr.
func TestRenderPlanSyncStepFailureIsRed(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, spec, st := runnerKubernetesDaemon(t, now)
	result := runnerTestResult(spec, now)
	cluster := &runnerCluster{t: t, status: "Complete", result: &result, failSyncCopy: true}
	cmds := &runnerFakeCmds{answer: cluster.answer}
	d.execCmd = cmds.run

	stage := d.tierSweepRunRunner(context.Background(), st, spec)

	if stage.reason != tierSweepRunnerNoResult {
		t.Fatalf("stage = %+v, want RED with reason %q", stage, tierSweepRunnerNoResult)
	}
	if stage.exitCode == 0 {
		t.Error("exit code 0 for a run whose sync stage never filled the repo claim")
	}
	if !strings.Contains(stage.output, "unable to copy into the sync pod") {
		t.Errorf("stage output does not carry the failed step's stderr:\n%s", stage.output)
	}
	plan, err := sweeprunner.Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the spec: %v", err)
	}
	for i := range cmds.ran {
		if bytes.Equal(cmds.stdin[i], plan.Job.JSON()) {
			t.Errorf("the Job was applied at command %q after the sync copy failed", cmds.ran[i])
		}
	}
}

// TestSyncPodIsCreatedAtomicallyAndTeardownCountIsExact drives a sync whose pod
// already exists: the create fails AlreadyExists, so the stage is RED carrying
// that stderr, no Job is applied, and the teardown's exact four removals run.
func TestSyncPodIsCreatedAtomicallyAndTeardownCountIsExact(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, spec, st := runnerKubernetesDaemon(t, now)
	result := runnerTestResult(spec, now)
	cluster := &runnerCluster{t: t, status: "Complete", result: &result, failSyncCreate: true}
	cmds := &runnerFakeCmds{answer: cluster.answer}
	d.execCmd = cmds.run

	stage := d.tierSweepRunRunner(context.Background(), st, spec)

	if stage.reason != tierSweepRunnerNoResult {
		t.Fatalf("stage = %+v, want RED with reason %q: a sync that never filled the claim has no run", stage, tierSweepRunnerNoResult)
	}
	if stage.exitCode == 0 {
		t.Error("exit code 0 for a run whose sync pod already existed")
	}
	if !strings.Contains(stage.output, "AlreadyExists") {
		t.Errorf("stage output does not carry the create's stderr:\n%s", stage.output)
	}
	if n := len(runnerCalls(cmds.ran, "create")); n != 1 {
		t.Errorf("creates = %d, want the one atomic create of the sync pod: %q", n, cmds.ran)
	}
	plan, err := sweeprunner.Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the spec: %v", err)
	}
	for i := range cmds.ran {
		if bytes.Equal(cmds.stdin[i], plan.Job.JSON()) {
			t.Errorf("the Job was applied at command %q after the sync pod's create failed", cmds.ran[i])
		}
	}
	if n := len(runnerCalls(cmds.ran, "delete")); n != 4 {
		t.Errorf("removals = %d, want the teardown's exact four and no sync stage removal: %q", n, cmds.ran)
	}
}

// TestTierSweepRunnerGuardsRefuseAMalformedGuard holds the guard loop to deny:
// a guard with no command, or one with no refusal to apply to its listing, is
// not a check that passed.
func TestTierSweepRunnerGuardsRefuseAMalformedGuard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		guard sweeprunner.Guard
		want  string
	}{
		{
			name:  "a guard with no command",
			guard: sweeprunner.Guard{Refuse: func([]byte) error { return nil }},
			want:  "no command",
		},
		{
			name:  "a guard with no refusal",
			guard: sweeprunner.Guard{Step: sweeprunner.Step{Argv: []string{"kubectl", "get", "jobs"}}},
			want:  "no refusal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{}
			out, code, err := d.tierSweepRunnerGuards(context.Background(), []sweeprunner.Guard{tc.guard})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("guards error = %v, want one naming %q", err, tc.want)
			}
			if code == 0 {
				t.Errorf("exit code = %d for a guard the daemon cannot run, want a refusal", code)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output does not carry %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestSyncRefusesWhileASweepJobReadsTheClaim drives the guard through a fake
// kubectl: a rig sweep Job still active refuses the run, naming it, and nothing
// is applied; a listing that cannot be read refuses too. A Job whose listing
// holds three true condition types ending in Complete is ended, not refused, so
// the run goes on in its order.
func TestSyncRefusesWhileASweepJobReadsTheClaim(t *testing.T) {
	t.Parallel()
	const activeJob = "gt-sweep-gastown-deadbeef-otherrun"
	for _, tc := range []struct {
		name string
		jobs string
		err  bool
		want string
	}{
		{name: "an unlabelled active Job", jobs: activeJob + "\n", want: activeJob},
		{name: "a true condition neither ended type", jobs: activeJob + " Suspended\n", want: activeJob},
		{name: "a listing that cannot be read", err: true, want: "the run's guard did not pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, spec, st := runnerKubernetesDaemon(t, now)
			result := runnerTestResult(spec, now)
			cluster := &runnerCluster{t: t, jobs: tc.jobs, jobsErr: tc.err, status: "Complete", result: &result}
			cmds := &runnerFakeCmds{answer: cluster.answer}
			d.execCmd = cmds.run

			stage := d.tierSweepRunRunner(context.Background(), st, spec)

			if stage.reason != tierSweepRunnerNoResult {
				t.Fatalf("stage = %+v, want RED with reason %q", stage, tierSweepRunnerNoResult)
			}
			if stage.exitCode == 0 {
				t.Error("exit code 0 for a run the guard refused")
			}
			if !strings.Contains(stage.output, tc.want) {
				t.Errorf("stage output does not name %q:\n%s", tc.want, stage.output)
			}
			for _, argv := range cmds.ran {
				if slices.Contains(strings.Fields(argv), "apply") || slices.Contains(strings.Fields(argv), "create") {
					t.Errorf("a step applied %q after the guard refused; nothing must be applied", argv)
				}
			}
			// The capacity preflight reads the node first and writes nothing; the
			// guard's listing of the Jobs follows and nothing is applied after it.
			guardAt := slices.IndexFunc(cmds.ran, func(ran string) bool {
				return slices.Contains(strings.Fields(ran), "jobs")
			})
			if guardAt < 0 {
				t.Fatalf("commands = %q, want the guard's listing of the Jobs", cmds.ran)
			}
			for _, ran := range cmds.ran[:guardAt] {
				if !slices.Contains(strings.Fields(ran), "nodes,pods") {
					t.Errorf("step %q ran before the guard's listing; only the read-only capacity preflight may", ran)
				}
			}
			if slices.Contains(strings.Fields(cmds.ran[guardAt]), "-l") {
				t.Errorf("the guard runs %q, want an unlabelled listing: a Job that lost its labels still reads the claim", cmds.ran[guardAt])
			}
		})
	}

	t.Run("three condition types ending in Complete", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, spec, st := runnerKubernetesDaemon(t, now)
		result := runnerTestResult(spec, now)
		cluster := &runnerCluster{
			t:      t,
			jobs:   "gt-sweep-gastown-deadbeef-donerun SuccessCriteriaMet Complete FailureTarget\n",
			status: "Complete",
			result: &result,
		}
		cmds := &runnerFakeCmds{answer: cluster.answer}
		d.execCmd = cmds.run

		stage := d.tierSweepRunRunner(context.Background(), st, spec)

		if stage.reason != "" {
			t.Fatalf("stage = %+v, want the ended Job to pass the guard and the copied result folded", stage)
		}
		if len(runnerCalls(cmds.ran, "create")) == 0 {
			t.Errorf("commands = %q, want the sync pod created after the guard passed", cmds.ran)
		}
	})
}

// A run's objects come down only after its result is read, whichever way the
// Job ended: the copy runs first, then the teardown removes the sync pod, the
// Job, the reader pod and the output claim. The repo claim every run reuses is
// never removed (gt-2j3t7).
func TestTierSweepRunnerKubernetesTeardown(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"Complete", "Failed"} {
		t.Run("the Job ended "+status, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, spec, st := runnerKubernetesDaemon(t, now)
			result := runnerTestResult(spec, now)
			cmds := &runnerFakeCmds{answer: (&runnerCluster{t: t, status: status, result: &result}).answer}
			d.execCmd = cmds.run

			stage := d.tierSweepRunRunner(context.Background(), st, spec)

			if stage.reason != "" {
				t.Fatalf("stage = %+v, want the copied result folded", stage)
			}
			// The sync stage copies into the repo claim; then the result and
			// its logs come home, the result first.
			copies := runnerCalls(cmds.ran, "cp")
			if len(copies) != 3 {
				t.Fatalf("copies = %d, want the sync copy, the result and the logs: %q", len(copies), cmds.ran)
			}
			lastCopy := copies[len(copies)-1]
			deletes := runnerCalls(cmds.ran, "delete")
			// The sync stage removes its pod itself, and the teardown removes
			// it again with --ignore-not-found along with the rest.
			if len(deletes) != 5 {
				t.Fatalf("removals = %d, want the sync stage's and the teardown's four: %q", len(deletes), cmds.ran)
			}
			teardown := deletes[len(deletes)-4:]
			for i, word := range []string{"pod", "job", "pod", "persistentvolumeclaim"} {
				at := teardown[i]
				if !slices.Contains(strings.Fields(cmds.ran[at]), word) {
					t.Errorf("teardown removal %d is %q, want one that removes a %s", i, cmds.ran[at], word)
				}
				if at < lastCopy {
					t.Errorf("teardown removal %q ran before the result and its logs were copied out", cmds.ran[at])
				}
			}
			for _, at := range deletes {
				if strings.Contains(cmds.ran[at], sweeprunner.RepoClaimName(spec.Rig)) {
					t.Errorf("a removal runs %q, which removes the repo claim every run reuses", cmds.ran[at])
				}
			}
		})
	}
}

// A Kubernetes run whose result the daemon cannot bring home is RED with the
// no-result reason, and its objects still come down, so no run is left behind a
// verdict the daemon has already taken (gt-2j3t7).
func TestTierSweepRunnerKubernetesRedAndTornDown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		status         string
		statusErr      bool
		noResult       bool
		skipWrite      bool
		failCopy       bool
		failWait       bool
		failSyncCreate bool
		failApply      int
		// wantDeletes is the teardown's four removals, plus the sync stage's
		// own removal when the launch reached it.
		wantDeletes int
	}{
		{name: "the Job ended with no result", status: "Failed", noResult: true, wantDeletes: 5},
		{name: "the status cannot be read", statusErr: true, wantDeletes: 5},
		{name: "the copy succeeds and writes nothing", status: "Complete", skipWrite: true, wantDeletes: 5},
		{name: "the copy fails", status: "Complete", failCopy: true, wantDeletes: 5},
		{name: "the reader pod never becomes ready", status: "Complete", failWait: true, wantDeletes: 5},
		{name: "the repo claim cannot be applied", status: "Complete", failApply: 1, wantDeletes: 4},
		{name: "the sync pod already exists", status: "Complete", failSyncCreate: true, wantDeletes: 4},
		{name: "the output claim cannot be applied", status: "Complete", failApply: 2, wantDeletes: 5},
		{name: "the Job cannot be applied", status: "Complete", failApply: 3, wantDeletes: 5},
		{name: "the reader pod cannot be applied", status: "Complete", failApply: 4, wantDeletes: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, spec, st := runnerKubernetesDaemon(t, now)
			cluster := &runnerCluster{t: t, status: tc.status, statusErr: tc.statusErr, skipWrite: tc.skipWrite, failCopy: tc.failCopy, failWait: tc.failWait, failSyncCreate: tc.failSyncCreate, failApply: tc.failApply}
			if !tc.noResult {
				result := runnerTestResult(spec, now)
				cluster.result = &result
			}
			cmds := &runnerFakeCmds{answer: cluster.answer}
			d.execCmd = cmds.run

			stage := d.tierSweepRunRunner(context.Background(), st, spec)

			if stage.reason != tierSweepRunnerNoResult {
				t.Fatalf("stage = %+v, want RED with reason %q", stage, tierSweepRunnerNoResult)
			}
			if stage.exitCode == 0 {
				t.Error("exit code 0 for a run whose result was never read; the cycle would record a clean sweep")
			}
			if tc.statusErr && !strings.Contains(stage.output, "the run's status cannot be read") {
				t.Errorf("stage output does not name the status read that failed for the whole budget:\n%s", stage.output)
			}
			// The teardown's four removals run whether or not the launch got as
			// far as its own sync-pod removal.
			if n := len(runnerCalls(cmds.ran, "delete")); n != tc.wantDeletes {
				t.Errorf("removals = %d, want %d, the teardown's four and the sync stage's own when the launch reached it: %q", n, tc.wantDeletes, cmds.ran)
			}
		})
	}
}

// One failed status read is not a verdict: a run that is going well can fail a
// read, and the poll asks again on its next tick, so the run's result is still
// folded and the run is torn down after it is read (gt-2j3t7).
func TestTierSweepRunnerKubernetesStatusReadRetried(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, spec, st := runnerKubernetesDaemon(t, now)
	result := runnerTestResult(spec, now)
	cluster := &runnerCluster{t: t, status: "Complete", result: &result, flakyStatus: 1}
	cmds := &runnerFakeCmds{answer: cluster.answer}
	d.execCmd = cmds.run
	d.tierSweepSeams.pollInterval = time.Millisecond

	stage := d.tierSweepRunRunner(context.Background(), st, spec)

	if stage.reason != "" {
		t.Fatalf("stage = %+v, want the copied result folded, not RED on the one failed read", stage)
	}
	if n := len(runnerCalls(cmds.ran, "get", "job")); n < 2 {
		t.Errorf("status reads = %d, want the poll to ask again after the failed one: %q", n, cmds.ran)
	}
	if !strings.Contains(stage.output, "integration GREEN passed=66") {
		t.Errorf("stage output does not carry the copied verdict:\n%s", stage.output)
	}
	if n := len(runnerCalls(cmds.ran, "delete")); n != 5 {
		t.Errorf("removals = %d, want the sync pod, the Job, the reader pod and the claim once the result was read, plus the sync stage's own removal: %q", n, cmds.ran)
	}
}

// tierSweepRunnerCollect is the step the poll takes each time the result is not
// on the host yet. It copies only a run that has ended, reports a run still going
// as not yet, and refuses what it cannot read or copy (gt-2j3t7).
func TestTierSweepRunnerCollect(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		status     string
		statusErr  bool
		noResult   bool
		backend    string
		noRunner   bool
		badSpec    bool
		wantCopied bool
		wantErr    bool
		wantCopy   bool
	}{
		{name: "a docker run writes its own result", backend: config.TierSweepRunnerDocker},
		{name: "a run still going is not copied", status: ""},
		{name: "a run that completed is copied", status: "Complete", wantCopied: true, wantCopy: true},
		{name: "a run that failed is copied", status: "Failed", wantCopied: true, wantCopy: true},
		{name: "a status that cannot be read is refused", statusErr: true, wantErr: true},
		{name: "a copy with no result in the claim is refused", status: "Failed", noResult: true, wantErr: true, wantCopy: true},
		{name: "a missing runner block is refused", noRunner: true, wantErr: true},
		{name: "a backend the daemon does not know is refused", backend: "podman", wantErr: true},
		{name: "a spec no backend renders is refused", badSpec: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, spec, _ := runnerKubernetesDaemon(t, now)
			cluster := &runnerCluster{t: t, status: tc.status, statusErr: tc.statusErr}
			if !tc.noResult {
				result := runnerTestResult(spec, now)
				cluster.result = &result
			}
			cmds := &runnerFakeCmds{answer: cluster.answer}
			d.execCmd = cmds.run
			runner := d.patrolConfig.Patrols.TierSweep.Runner
			switch {
			case tc.backend != "":
				runner.Backend = tc.backend
			case tc.noRunner:
				d.patrolConfig.Patrols.TierSweep.Runner = nil
			case tc.badSpec:
				spec.CPUs = 0
			}
			if err := os.MkdirAll(spec.OutDir, 0o700); err != nil {
				t.Fatal(err)
			}

			copied, err := d.tierSweepRunnerCollect(context.Background(), spec)

			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want an error = %v", err, tc.wantErr)
			}
			if copied != tc.wantCopied {
				t.Errorf("copied = %v, want %v", copied, tc.wantCopied)
			}
			if got := len(runnerCalls(cmds.ran, "cp")) > 0; got != tc.wantCopy {
				t.Errorf("a copy ran = %v, want %v: %q", got, tc.wantCopy, cmds.ran)
			}
			if tc.wantCopied {
				if _, err := sweeprunner.Read(spec.OutDir, spec.Rig, spec.SHA); err != nil {
					t.Errorf("the copied result does not read back: %v", err)
				}
			}
		})
	}
}

// TestTierSweepRunnerHostSummary pins the one mapping the recorded line has:
// a log directory under the engine's OutputDir moves to the same relative path
// in the host run directory; a line with no group, or a directory the backend
// does not bring home, is left as printed (gt-4lj2q). A path that walks up out
// of OutputDir with ".." is left as printed too: joining it would name a host
// path outside the run directory (gt-s7idr).
func TestTierSweepRunnerHostSummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		summary string
		outDir  string
		want    string
	}{
		{
			name:    "a run path moves to the host run directory",
			summary: "tier-sweep: integration RED passed=1 failed=1 skipped=0 failed: ./internal/cmd (logs /out/logs/integration) in 2m1s",
			outDir:  "/host/runs/gastown-abc",
			want:    "tier-sweep: integration RED passed=1 failed=1 skipped=0 failed: ./internal/cmd (logs /host/runs/gastown-abc/logs/integration) in 2m1s",
		},
		{
			name:    "a line with no log group",
			summary: "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 in 4s",
			outDir:  "/host/runs/gastown-abc",
			want:    "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 in 4s",
		},
		{
			name:    "a directory outside the engine's output",
			summary: "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x) in 4s",
			outDir:  "/host/runs/gastown-abc",
			want:    "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x) in 4s",
		},
		{
			name:    "a path that walks up out of the engine's output",
			summary: "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /out/../etc) in 4s",
			outDir:  "/host/runs/gastown-abc",
			want:    "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /out/../etc) in 4s",
		},
		{
			name:    "a path that walks up after a subdirectory",
			summary: "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /out/logs/../../x) in 4s",
			outDir:  "/host/runs/gastown-abc",
			want:    "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /out/logs/../../x) in 4s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tierSweepRunnerHostSummary(tc.summary, tc.outDir); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

// A run that wrote no logs leaves the reader's /out/logs absent, and the copy
// of it fails with "no such file": that is not the run's failure, so the copied
// result's verdicts stand and the collect reports no error. Any other copy
// failure is still the run's (gt-4lj2q).
func TestK8sCollectToleratesNoLogsDirectory(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, spec, st := runnerKubernetesDaemon(t, now)
	result := runnerTestResult(spec, now)
	cluster := &runnerCluster{t: t, status: "Complete", result: &result, noLogs: true}
	cmds := &runnerFakeCmds{answer: cluster.answer}
	d.execCmd = cmds.run

	stage := d.tierSweepRunRunner(context.Background(), st, spec)

	if stage.reason != "" {
		t.Fatalf("stage = %+v, want the copied result folded with no error: an absent logs directory is not a failure", stage)
	}
	for _, want := range []string{"integration GREEN passed=66", "race RED"} {
		if !strings.Contains(stage.output, want) {
			t.Errorf("stage output does not carry the copied verdict %q:\n%s", want, stage.output)
		}
	}
	if stage.exitCode != tierSweepRedExit {
		t.Errorf("exit code = %d, want the sweep's RED exit: the race tier is RED", stage.exitCode)
	}
	if _, err := sweeprunner.Read(spec.OutDir, spec.Rig, spec.SHA); err != nil {
		t.Errorf("the result was not brought home past the absent logs directory: %v", err)
	}
	if n := len(runnerCalls(cmds.ran, "cp")); n != 3 {
		t.Errorf("copies = %d, want the sync copy, the result and the attempted logs copy: %q", n, cmds.ran)
	}
}

// The summary the daemon records names the host directory the logs were copied
// to, not the engine's /out/logs, which the teardown removes with the claim
// (gt-4lj2q).
func TestTierSweepSummaryNamesTheHostLogPath(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	const shellSummary = "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n"
	cfg := runnerTestConfig(t)
	cfg.Backend = config.TierSweepRunnerKubernetes
	d, _ := runnerCycleDaemon(t, now, cfg, shellSummary)
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Second}
	spec, ok, err := d.tierSweepRunnerSpec(st)
	if err != nil || !ok {
		t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
	}
	result := runnerTestResult(spec, now)
	cmds := &runnerFakeCmds{answer: (&runnerCluster{t: t, status: "Complete", result: &result}).answer}
	d.execCmd = cmds.run

	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()
	if !d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
		t.Fatal("the cycle deferred; both stages ran and reported")
	}

	race := tierSweepTestState(t, d, "gastown").Tiers["race"]
	if race.Verdict != tierSweepRed {
		t.Fatalf("race = %+v, want the RED the result names", race)
	}
	want := filepath.Join(spec.OutDir, sweeprunner.LogsDirName)
	if race.LogDir != want {
		t.Errorf("race log dir = %q, want the host run directory %q", race.LogDir, want)
	}
	if strings.HasPrefix(race.LogDir, sweeprunner.OutputDir+"/") {
		t.Errorf("race log dir = %q, want it away from the engine's %s: the teardown deletes that claim", race.LogDir, sweeprunner.OutputDir)
	}
	if out, err := os.ReadFile(race.Log); err != nil {
		t.Errorf("reading the stage log %q: %v", race.Log, err)
	} else if !strings.Contains(string(out), "tier-sweep: race RED") {
		t.Errorf("the stage log %q does not hold the result's summary line:\n%s", race.Log, out)
	}
}

// A cycle that ends while the run's status is being read defers the stage: the
// run is left going on its cluster for the next cycle to adopt, so its objects
// are not removed and nothing is judged (gt-2j3t7).
func TestTierSweepRunnerKubernetesCycleEndsDuringCollect(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, spec, st := runnerKubernetesDaemon(t, now)
	cluster := &runnerCluster{t: t, statusErr: true}
	cmds := &runnerFakeCmds{answer: cluster.answer}
	d.execCmd = cmds.run
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stage := d.tierSweepRunRunner(ctx, st, spec)

	if !errors.Is(stage.err, context.Canceled) {
		t.Fatalf("stage = %+v, want the cycle's cancellation returned so the cycle defers", stage)
	}
	if stage.reason != "" {
		t.Errorf("stage reason = %q, want none: a cycle that ended the wait learned nothing about the tree", stage.reason)
	}
	// The sync stage removed its own pod while filling the claim; the run's Job
	// and its output claim must stay for the next cycle to adopt.
	for _, argv := range cmds.ran {
		fields := strings.Fields(argv)
		if !slices.Contains(fields, "delete") {
			continue
		}
		if slices.Contains(fields, "job") || slices.Contains(fields, sweeprunner.OutClaimName(spec)) {
			t.Errorf("the run's objects were removed on a cycle that only ended the wait: %q", argv)
		}
	}
}

// runnerPriorityDeclineListing is what the priority preflight answers while a
// Forgejo pod runs at priority 0 under the sweep's class at 100: the run would
// outrank that pod.
const runnerPriorityDeclineListing = `{"kind":"List","items":[
{"kind":"PriorityClass","metadata":{"name":"gt-ci"},"value":100},
{"kind":"PriorityClass","metadata":{"name":"gt-ci-gate"},"value":200},
{"kind":"PriorityClass","metadata":{"name":"gt-control-plane"},"value":1000000},
{"kind":"Pod","metadata":{"name":"forgejo-abc","namespace":"forgejo"},"spec":{"priority":0}}]}`

// A preflight decline is not a failed run: nothing was attempted and nothing
// was learned about the tree, so the stage reports that it did not run and the
// cycle defers. The line naming the pod is logged at most once an hour while
// the reason stands, and at once again when the preflight clears and the
// decline re-arises inside that hour. The decline comes from the priority
// guard over the launcher's kubectl listing, the path a cycle really takes
// (gt-gg9hv.39).
func TestTierSweepRunnerPriorityDeclineDefers(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
	cfg := runnerTestConfig(t)
	cfg.Backend = config.TierSweepRunnerKubernetes
	d.patrolConfig.Patrols.TierSweep.Runner = cfg
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}
	spec, ok, err := d.tierSweepRunnerSpec(st)
	if err != nil || !ok {
		t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
	}
	result := runnerTestResult(spec, now)
	cluster := &runnerCluster{t: t, status: "Complete", result: &result, priority: []byte(runnerPriorityDeclineListing)}
	d.execCmd = (&runnerFakeCmds{answer: cluster.answer}).run

	const line = "tier_sweep: not launching: pod forgejo/forgejo-abc"
	cycle := func() tierSweepStageResult {
		t.Helper()
		return d.tierSweepRunRunner(context.Background(), st, spec)
	}
	declined := func() {
		t.Helper()
		stage := cycle()
		if stage.ran {
			t.Fatalf("stage = %+v, want ran=false so the cycle defers rather than going red", stage)
		}
		if stage.err == nil {
			t.Error("the declined stage reports no error for the cycle's deferral line")
		}
	}

	declined()
	if got := strings.Count(logs.String(), line); got != 1 {
		t.Fatalf("the decline line appears %d times after the first cycle, want once:\n%s", got, logs.String())
	}

	// A second cycle inside the hour retries the launch and says nothing new.
	declined()
	if got := strings.Count(logs.String(), line); got != 1 {
		t.Errorf("the decline line appears %d times inside the hour, want it once:\n%s", got, logs.String())
	}

	clk, ok := d.clock.(*clockwork.FakeClock)
	if !ok {
		t.Fatalf("test clock = %T, want a fake clock to advance", d.clock)
	}

	// The preflight passes: the same launch path starts the run, so no decline
	// stands. Its result is removed so the next cycle launches instead of
	// folding this run's result.
	cluster.priority = nil
	clk.Advance(time.Minute)
	if stage := cycle(); !stage.ran {
		t.Fatalf("stage = %+v, want the run launched once the preflight passed", stage)
	}
	if err := os.Remove(filepath.Join(spec.OutDir, sweeprunner.ResultFileName)); err != nil {
		t.Fatalf("removing the launched run's result for the next cycle: %v", err)
	}

	// The decline re-arises inside the hour: it logs at once.
	cluster.priority = []byte(runnerPriorityDeclineListing)
	clk.Advance(time.Minute)
	declined()
	if got := strings.Count(logs.String(), line); got != 2 {
		t.Errorf("the decline line appears %d times after it cleared and re-arose, want it logged again at once:\n%s", got, logs.String())
	}

	// It still stands: the next report waits out the hour from that line.
	clk.Advance(30 * time.Minute)
	declined()
	if got := strings.Count(logs.String(), line); got != 2 {
		t.Errorf("the standing decline was logged again inside the hour:\n%s", logs.String())
	}
	clk.Advance(31 * time.Minute)
	declined()
	if got := strings.Count(logs.String(), line); got != 3 {
		t.Errorf("the decline line appears %d times after the hour, want the standing decline reported again:\n%s", got, logs.String())
	}
}
