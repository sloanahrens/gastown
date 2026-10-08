package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// The runner-idle wait: before a stage that takes the container-gate slot
// starts, the sweep waits, bounded, for the CI runner to report idle, and says
// in its log when it gave up and ran under load anyway (gt-5ejux). Every test
// here drives the wait through the daemon's seams — the probe and the pause —
// over a fake clock, so none of them sleeps on the wall.

const idleTestSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// newRunnerIdleDaemon is an even-hour sweep of gastown whose runner-idle probe
// answers from probe (poll 1, 2, ...) and whose pauses advance the fake clock
// by exactly the pause they were asked for.
func newRunnerIdleDaemon(t *testing.T, wait string, probe func(poll int) error) (*Daemon, *tierSweepRunRecorder, *bytes.Buffer, *clockwork.FakeClock) {
	t.Helper()
	// 14:00 is even, so the cycle runs the shell stage and then the integration
	// suite: the one stage that takes the container-gate slot.
	now := atHour(time.Now(), 14)
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	clk := clockwork.NewFakeClockAt(now)
	d.clock = clk
	rec.green()
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return idleTestSHA, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	cfg := d.patrolConfig.Patrols.TierSweep
	cfg.RunnerIdleCommand = "gt ci runner-idle"
	cfg.RunnerIdleWaitStr = wait
	polls := 0
	d.tierSweepSeams.runnerIdle = func(context.Context, string, string) error {
		polls++
		if probe == nil {
			return nil
		}
		return probe(polls)
	}
	d.tierSweepSeams.idleSleep = func(_ context.Context, pause time.Duration) error {
		clk.Advance(pause)
		return nil
	}
	return d, rec, logs, clk
}

// busyN is a probe that reports busy for n polls, then idle.
func busyN(n int, err error) func(poll int) error {
	return func(poll int) error {
		if poll <= n {
			return err
		}
		return nil
	}
}

// Without runner_idle_command the cycle is exactly what it was before the key
// existed: no probe runs, no wait is logged, and the verdict summary is the
// plain one (gt-5ejux, acceptance 1).
func TestTierSweepRunnerIdle_UnsetCommandWaitsForNothing(t *testing.T) {
	t.Parallel()
	d, rec, logs, _ := newRunnerIdleDaemon(t, "", nil)
	d.patrolConfig.Patrols.TierSweep.RunnerIdleCommand = ""
	d.tierSweepSeams.runnerIdle = func(context.Context, string, string) error {
		t.Fatal("the sweep probed the CI runner with runner_idle_command unset")
		return nil
	}
	d.tierSweepSeams.idleSleep = func(context.Context, time.Duration) error {
		t.Fatal("the sweep paused for a runner-idle wait with runner_idle_command unset")
		return nil
	}

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}
	if len(rec.stages) != 2 {
		t.Fatalf("stages = %+v, want the shell and integration stages", rec.stages)
	}
	out := logs.String()
	if strings.Contains(out, "under load") || strings.Contains(out, "CI runner") {
		t.Errorf("an unconfigured sweep logged a runner-idle wait; got:\n%s", out)
	}
	if want := "tier_sweep: gastown: swept aaaaaaaa (shell GREEN, integration GREEN, race GREEN) in "; !strings.Contains(out, want) {
		t.Errorf("log missing the unchanged verdict summary %q; got:\n%s", want, out)
	}
}

// A runner that reports busy and then idle holds the stage until it is idle:
// the wait polls every 15s and the stage starts only after the idle report
// (gt-5ejux, acceptance 2).
func TestTierSweepRunnerIdle_WaitsUntilTheRunnerReportsIdle(t *testing.T) {
	t.Parallel()
	d, rec, logs, _ := newRunnerIdleDaemon(t, "", busyN(3, errors.New("busy")))
	pauses := []time.Duration{}
	d.tierSweepSeams.idleSleep = func(ctx context.Context, pause time.Duration) error {
		pauses = append(pauses, pause)
		d.clock.(*clockwork.FakeClock).Advance(pause)
		return nil
	}

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}
	if len(pauses) != 3 {
		t.Fatalf("pauses = %v, want one per busy poll before the idle one", pauses)
	}
	for _, p := range pauses {
		if p != tierSweepRunnerIdlePoll {
			t.Errorf("pause = %v, want the 15s poll interval", p)
		}
	}
	if len(rec.stages) != 2 || strings.Join(rec.stages[1].tiers, " ") != "integration race" {
		t.Fatalf("stages = %+v, want the integration suite to have run", rec.stages)
	}
	out := logs.String()
	if !strings.Contains(out, "tier_sweep: gastown: integration stage: the CI runner reported idle after 45s") {
		t.Errorf("log missing the idle report; got:\n%s", out)
	}
	if strings.Contains(out, "under load") {
		t.Errorf("an idle report read as a load; got:\n%s", out)
	}
}

// A runner that never reports idle starts the stage after runner_idle_wait
// anyway, and both the wait's log line and the cycle's verdict summary say it
// ran under load. The under-load verdict is otherwise an ordinary one: a green
// one still counts as the complete sweep (gt-5ejux, acceptance 3).
func TestTierSweepRunnerIdle_TimesOutAndSaysItRanUnderLoad(t *testing.T) {
	t.Parallel()
	d, rec, logs, clk := newRunnerIdleDaemon(t, "45s", func(int) error { return errors.New("runner busy") })
	d.tierSweepSeams.idleSleep = func(ctx context.Context, pause time.Duration) error {
		clk.Advance(pause)
		return nil
	}

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}
	if len(rec.stages) != 2 {
		t.Fatalf("stages = %+v, want the integration suite to have started after the cap", rec.stages)
	}
	out := logs.String()
	for _, want := range []string{
		"tier_sweep: gastown: integration stage: the CI runner was still busy after 45s; starting it under load (last check: runner busy)",
		"(shell GREEN, integration GREEN under load, race GREEN under load) in ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got:\n%s", want, out)
		}
	}
	if st := tierSweepTestState(t, d, "gastown"); st.LastGreenSHA != idleTestSHA {
		t.Errorf("LastGreenSHA = %q, want %s: an under-load sweep is an ordinary verdict", st.LastGreenSHA, idleTestSHA)
	}
}

// The wait honors a canceled context promptly, and a canceled wait starts no
// stage: the cycle defers instead, and no green is recorded for a sweep that
// never covered its tiers (gt-5ejux, acceptance 4).
func TestTierSweepRunnerIdle_CancelStopsTheWaitAndTheStage(t *testing.T) {
	t.Parallel()
	d, rec, logs, _ := newRunnerIdleDaemon(t, "1h", func(int) error { return errors.New("busy") })
	d.tierSweepSeams.idleSleep = func(context.Context, time.Duration) error {
		// What the real pause returns when the wait's context is canceled.
		return context.Canceled
	}

	if d.runTierSweep() {
		t.Error("runTierSweep reached a verdict; a canceled wait learned nothing about the tree")
	}
	for _, st := range rec.stages {
		if st.slot {
			t.Errorf("stages = %+v, want no slot stage after a canceled wait", rec.stages)
		}
	}
	if !strings.Contains(logs.String(), "the runner-idle wait stopped (context canceled); deferring") {
		t.Errorf("log missing the canceled wait; got:\n%s", logs.String())
	}
	if st := tierSweepTestState(t, d, "gastown"); st.LastGreenSHA != "" {
		t.Errorf("LastGreenSHA = %q, want empty: the integration tier never ran", st.LastGreenSHA)
	}
}

// The wait itself stops the moment its context is canceled rather than polling
// out its cap (gt-5ejux, acceptance 4).
func TestTierSweepAwaitRunnerIdle_CancelStopsThePolling(t *testing.T) {
	t.Parallel()
	d, _, _, _ := newRunnerIdleDaemon(t, "1h", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polls := 0
	d.tierSweepSeams.runnerIdle = func(context.Context, string, string) error {
		polls++
		return errors.New("busy")
	}
	d.tierSweepSeams.idleSleep = func(context.Context, time.Duration) error {
		// The daemon drains: the next pass must see the canceled context, not
		// another poll.
		cancel()
		return nil
	}

	_, err := d.tierSweepAwaitRunnerIdle(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want context.Canceled", err)
	}
	if polls != 1 {
		t.Errorf("polls = %d, want one: the wait must stop at the cancel, not at the cap", polls)
	}
}

// The wait runs before the stage that takes the container-gate slot, and never
// while that stage holds it: land.WithSlot takes the slot inside the stage run,
// so a wait that has ended before the stage starts was spent holding nothing
// (gt-5ejux, acceptance 5).
func TestTierSweepRunnerIdle_WaitsBeforeTheSlotStageStarts(t *testing.T) {
	t.Parallel()
	d, rec, _, _ := newRunnerIdleDaemon(t, "", busyN(2, errors.New("busy")))
	var events []string
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		return tierSweepStageResult{
			output:   "tier-sweep: " + strings.Join(st.tiers, " ") + " GREEN passed=1 failed=0 skipped=0\n",
			exitCode: 0,
			ran:      true,
		}
	}
	inner := d.tierSweepSeams.runnerIdle
	d.tierSweepSeams.runnerIdle = func(ctx context.Context, dir, command string) error {
		events = append(events, "poll")
		return inner(ctx, dir, command)
	}
	d.tierSweepSeams.idleSleep = func(ctx context.Context, pause time.Duration) error {
		events = append(events, "pause")
		d.clock.(*clockwork.FakeClock).Advance(pause)
		return nil
	}
	d.tierSweepSeams.run = func(ctx context.Context, dir string, st tierSweepStage) tierSweepStageResult {
		events = append(events, "run:"+strings.Join(st.tiers, "+"))
		return rec.run(ctx, dir, st)
	}

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}
	joined := strings.Join(events, " ")
	if joined != "run:shell poll pause poll pause poll run:integration+race" {
		t.Fatalf("events = %q, want every wait event before the slot stage's run", joined)
	}
	if last := events[len(events)-1]; last != "run:integration+race" {
		t.Errorf("last event = %q, want the slot stage's run: no wait may follow it", last)
	}
	if len(rec.stages) != 2 || !rec.stages[1].slot {
		t.Fatalf("stages = %+v, want the second one to be the slot stage", rec.stages)
	}
}

// The two keys and their defaults: no command means no wait, and an unset or
// unparsable runner_idle_wait means 10m (gt-5ejux, acceptance 6).
func TestTierSweepRunnerIdleConfig(t *testing.T) {
	t.Parallel()
	if got := tierSweepRunnerIdleCommand(nil); got != "" {
		t.Errorf("command with no config = %q, want empty", got)
	}
	if got := tierSweepRunnerIdleWait(nil); got != 10*time.Minute {
		t.Errorf("wait with no config = %v, want 10m", got)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{
		RunnerIdleCommand: " gt ci runner-idle ",
		RunnerIdleWaitStr: "30s",
	}}}
	if got := tierSweepRunnerIdleCommand(cfg); got != "gt ci runner-idle" {
		t.Errorf("command = %q, want the trimmed command", got)
	}
	if got := tierSweepRunnerIdleWait(cfg); got != 30*time.Second {
		t.Errorf("wait = %v, want 30s", got)
	}
	cfg.Patrols.TierSweep.RunnerIdleWaitStr = "not a duration"
	if got := tierSweepRunnerIdleWait(cfg); got != 10*time.Minute {
		t.Errorf("wait with an unparsable value = %v, want the 10m default", got)
	}
}
