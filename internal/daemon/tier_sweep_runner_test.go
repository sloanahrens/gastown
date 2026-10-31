package daemon

import (
	"context"
	"os"
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

// A result that never arrives, or arrives torn, judged nothing: the integration
// and race tiers are RED with the runner's own reason, never GREEN, and the
// teardown still runs so the containers are not left behind (gt-z4ppb).
func TestTierSweepRunnerMissingResultIsRed(t *testing.T) {
	t.Parallel()

	t.Run("no result after the budget", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, _ := runnerCycleDaemon(t, now, runnerTestConfig(t), "")
		stopped := false
		d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
			return tierSweepLaunch{exitCode: 0, stop: func() { stopped = true }}
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
		if !stopped {
			t.Error("the run's teardown did not run; its containers would be left behind")
		}
	})

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
