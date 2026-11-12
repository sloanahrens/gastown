package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A SIGTERM during a landing step waits for that step to finish, and starts no
// new landing pass while it waits: the pass in flight is the one the drain waits
// on, and a pass asked for during the drain is refused (gt-99ys4n).
func TestShutdownDrainWaitsForTheLandingStepAndStartsNothingNew(t *testing.T) {
	t.Parallel()
	d, _ := signalTestDaemon(t)
	d.drainBound = time.Hour
	if !d.beginLandingPass() {
		t.Fatalf("%s: no landing pass could start before the signal", t.Name())
	}
	holdTaken := make(chan struct{})
	d.onDrainHold = func() { close(holdTaken) }
	sigChan := make(chan os.Signal, 1)
	drained := make(chan os.Signal, 1)
	go func() { drained <- d.drainForShutdown(sigChan) }()

	reached(t, "the drain taking its hold", holdTaken)
	select {
	case sig := <-drained:
		t.Fatalf("%s: the drain returned %v while the landing pass was in flight", t.Name(), sig)
	default:
	}
	if !d.landingPassDraining() {
		t.Errorf("%s: the drain holds no landing passes off, so the pass in flight could claim new beads", t.Name())
	}
	if !d.specDispatchParked() {
		t.Errorf("%s: a spec-dispatch tick may start during the drain", t.Name())
	}
	if d.beginLandingPass() {
		t.Errorf("%s: a landing pass started during the drain", t.Name())
	}

	d.landingPassEnded()
	if sig := within(t, signalWait, "the drain after the landing pass ends", drained); sig != nil {
		t.Errorf("%s: the drain returned the signal %v, want nil once the pass ended", t.Name(), sig)
	}
}

// An idle daemon has no landing step to wait on, so a SIGTERM ends the drain at
// once, well inside the bound.
func TestShutdownDrainIdleDaemonExitsPromptly(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	d.drainBound = time.Hour
	drained := make(chan os.Signal, 1)
	go func() { drained <- d.drainForShutdown(make(chan os.Signal)) }()

	if sig := within(t, signalWait, "the drain of an idle daemon", drained); sig != nil {
		t.Errorf("%s: the drain returned the signal %v, want nil", t.Name(), sig)
	}
	if !strings.Contains(logs.String(), "no landing pass in flight") {
		t.Errorf("%s: the drain did not say it found nothing to wait for:\n%s", t.Name(), logs)
	}
}

// A second shutdown signal during the wait ends it at once, with the pass still
// in flight.
func TestShutdownDrainSecondSignalEndsTheWait(t *testing.T) {
	t.Parallel()
	d, _ := signalTestDaemon(t)
	d.drainBound = time.Hour
	if !d.beginLandingPass() {
		t.Fatalf("%s: no landing pass could start before the signal", t.Name())
	}
	holdTaken := make(chan struct{})
	d.onDrainHold = func() { close(holdTaken) }
	sigChan := make(chan os.Signal, 1)
	drained := make(chan os.Signal, 1)
	go func() { drained <- d.drainForShutdown(sigChan) }()

	reached(t, "the drain taking its hold", holdTaken)
	sigChan <- syscall.SIGTERM
	if sig := within(t, signalWait, "the drain after a second signal", drained); sig != syscall.SIGTERM {
		t.Errorf("%s: the drain returned %v, want the second SIGTERM", t.Name(), sig)
	}
}

// A landing step that never finishes is abandoned at the bound, and the drain
// says so.
func TestShutdownDrainBoundEndsAPassThatNeverFinishes(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	d.drainBound = 10 * time.Millisecond
	if !d.beginLandingPass() {
		t.Fatalf("%s: no landing pass could start before the signal", t.Name())
	}
	drained := make(chan os.Signal, 1)
	go func() { drained <- d.drainForShutdown(make(chan os.Signal)) }()

	if sig := within(t, signalWait, "the drain past its bound", drained); sig != nil {
		t.Errorf("%s: the drain returned the signal %v, want nil at the bound", t.Name(), sig)
	}
	if !strings.Contains(logs.String(), "still in flight after") {
		t.Errorf("%s: the drain did not report the abandoned pass:\n%s", t.Name(), logs)
	}
}

// SIGUSR1 and SIGUSR2 during the wait are ignored, as the main loop ignores them,
// so the wait goes on. The SIGTERM queued behind them ends it, which proves both
// were consumed and not taken as the second signal.
func TestShutdownDrainIgnoresLifecycleSignalsWhileWaiting(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	d.drainBound = time.Hour
	if !d.beginLandingPass() {
		t.Fatalf("%s: no landing pass could start before the signal", t.Name())
	}
	sigChan := make(chan os.Signal, 3)
	sigChan <- syscall.SIGUSR1
	sigChan <- syscall.SIGUSR2
	sigChan <- syscall.SIGTERM
	drained := make(chan os.Signal, 1)
	go func() { drained <- d.drainForShutdown(sigChan) }()

	if sig := within(t, signalWait, "the drain behind the lifecycle signals", drained); sig != syscall.SIGTERM {
		t.Errorf("%s: the drain returned %v, want the SIGTERM after the ignored signals", t.Name(), sig)
	}
	for _, line := range []string{"Received SIGUSR1 while draining: ignored", "Received reload-restart signal while draining: ignored"} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("%s: the drain did not log %q:\n%s", t.Name(), line, logs)
		}
	}
	d.landingPassEnded()
}

// A SIGTERM during a heartbeat drains the landing pass before the daemon context
// is canceled: the cancel would otherwise cut the landing step. The heartbeat
// itself is not released until the drain ends.
func TestShutdownDrainHeartbeatCancelsOnlyAfterTheLandingStep(t *testing.T) {
	t.Parallel()
	d, _ := signalTestDaemon(t)
	d.drainBound = time.Hour
	d.hbGrace = 20 * time.Millisecond
	if !d.beginLandingPass() {
		t.Fatalf("%s: no landing pass could start before the signal", t.Name())
	}
	holdTaken := make(chan struct{})
	d.onDrainHold = func() { close(holdTaken) }
	release := make(chan struct{})
	defer close(release)
	sigChan := make(chan os.Signal, 1)
	result := make(chan os.Signal, 1)
	go func() { result <- d.runHeartbeat(sigChan, func() { <-release }) }()

	sigChan <- syscall.SIGTERM
	reached(t, "the heartbeat's drain taking its hold", holdTaken)
	if err := d.ctx.Err(); err != nil {
		t.Fatalf("%s: the daemon context was canceled (%v) while the landing pass was in flight", t.Name(), err)
	}

	d.landingPassEnded()
	if sig := within(t, signalWait, "the heartbeat after the drain", result); sig != syscall.SIGTERM {
		t.Errorf("%s: runHeartbeat returned %v, want the SIGTERM", t.Name(), sig)
	}
	if err := d.ctx.Err(); err == nil {
		t.Errorf("%s: the daemon context is still live after the drain", t.Name())
	}
}

// A second SIGTERM during a heartbeat's drain ends the wait at once: the grace is
// an hour here, so only the second-signal path returns in time.
func TestShutdownDrainSecondSignalDuringHeartbeatEndsAtOnce(t *testing.T) {
	t.Parallel()
	d, _ := signalTestDaemon(t)
	d.drainBound = time.Hour
	d.hbGrace = time.Hour
	if !d.beginLandingPass() {
		t.Fatalf("%s: no landing pass could start before the signal", t.Name())
	}
	holdTaken := make(chan struct{})
	d.onDrainHold = func() { close(holdTaken) }
	release := make(chan struct{})
	defer close(release)
	sigChan := make(chan os.Signal, 2)
	result := make(chan os.Signal, 1)
	go func() { result <- d.runHeartbeat(sigChan, func() { <-release }) }()

	sigChan <- syscall.SIGTERM
	reached(t, "the heartbeat's drain taking its hold", holdTaken)
	sigChan <- syscall.SIGTERM
	if sig := within(t, signalWait, "the heartbeat after a second signal", result); sig != syscall.SIGTERM {
		t.Errorf("%s: runHeartbeat returned %v, want the SIGTERM", t.Name(), sig)
	}
	d.landingPassEnded()
}

// The run loop's SIGTERM branch drains too: shutdownForSignal waits for the
// landing pass in flight, then saves the state as stopped.
func TestShutdownDrainRunLoopSigtermWaitsForThePass(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	d.drainBound = time.Hour
	if !d.beginLandingPass() {
		t.Fatalf("%s: no landing pass could start before the signal", t.Name())
	}
	holdTaken := make(chan struct{})
	d.onDrainHold = func() { close(holdTaken) }
	state := &State{Running: true}
	sigChan := make(chan os.Signal, 1)
	shut := make(chan error, 1)
	go func() { shut <- d.shutdownForSignal(syscall.SIGTERM, state, sigChan) }()

	reached(t, "the run loop's drain taking its hold", holdTaken)
	select {
	case err := <-shut:
		t.Fatalf("%s: shutdown returned (%v) while the landing pass was in flight", t.Name(), err)
	default:
	}
	d.landingPassEnded()
	if err := within(t, signalWait, "the shutdown after the landing pass ends", shut); err != nil && !errors.Is(err, ErrUnrequestedStop) {
		t.Fatalf("%s: shutdownForSignal: %v", t.Name(), err)
	}
	if state.Running {
		t.Errorf("%s: state.Running is still true after the drain", t.Name())
	}
	if !strings.Contains(logs.String(), "landing pass finished") {
		t.Errorf("%s: the drain did not log that the pass finished:\n%s", t.Name(), logs)
	}
}

// A tier sweep triggered while the daemon drains starts no cycle and records no
// run, so the next daemon's first trigger still owes the sweep.
func TestShutdownDrainTierSweepStartsNoCycle(t *testing.T) {
	t.Parallel()
	d, rec, _, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	rec.green()
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.draining.Store(true)

	d.triggerTierSweep()
	waitTierSweepIdle(t, d)
	if len(rec.stages) != 0 {
		t.Errorf("%s: stages = %+v, want none while the daemon drains", t.Name(), rec.stages)
	}
	if _, found, err := loadPatrolLastRun(d.config.TownRoot, "tier_sweep"); err != nil || found {
		t.Errorf("%s: last run while draining: found=%v err=%v, want none recorded", t.Name(), found, err)
	}
}

// A SIGTERM with a sling in flight waits for that sling within the drain's bound,
// and starts none that was queued behind it: the sling in flight finishes and is
// counted, and the one waiting for its slot is reported back as not started, so
// the drain never runs a sling the cut would kill (gt-d4jrdc).
func TestShutdownDrainWaitsForASlingAndStartsNoQueuedOne(t *testing.T) {
	t.Parallel()
	b, fake, d, lines := newBlockedSlings(t, plannedTick(
		slingJob("gt-a", "claude-sonnet"),
		slingJob("gt-b", "deepseek-flash")))
	if err := SavePatrolConfig(d.config.TownRoot, &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		SpecDispatch: &SpecDispatchConfig{MaxInFlight: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	d.drainBound = time.Hour
	holdTaken := make(chan struct{})
	d.onDrainHold = func() { close(holdTaken) }

	d.runSpecDispatch()
	recvSling(t, b.started)
	drained := make(chan os.Signal, 1)
	go func() { drained <- d.drainForShutdown(make(chan os.Signal)) }()

	reached(t, "the drain taking its hold", holdTaken)
	select {
	case sig := <-drained:
		t.Fatalf("%s: the drain returned %v while gt-a was in flight", t.Name(), sig)
	default:
	}
	close(b.release)
	if sig := within(t, signalWait, "the drain after the sling returns", drained); sig != nil {
		t.Errorf("%s: the drain returned the signal %v, want nil once the sling returned", t.Name(), sig)
	}
	waitSlings(t, d)

	if n := len(fake.argvs("spec", "sling")); n != 1 {
		t.Errorf("%s: slings run = %d, want gt-a alone: the queued gt-b must not start", t.Name(), n)
	}
	if n := countContaining(lines(), "gt-b: sling not started: the daemon shut down before the sling had a slot"); n != 1 {
		t.Errorf("%s: gt-b not-started lines = %d, want one: %v", t.Name(), n, lines())
	}
	if in := d.specSlings.list(); len(in) != 0 {
		t.Errorf("%s: slings in flight after the drain = %+v, want none", t.Name(), in)
	}
	d.dispatchTicksMu.Lock()
	defer d.dispatchTicksMu.Unlock()
	if len(d.dispatchTicks) != 1 {
		t.Fatalf("%s: tick records = %d, want one", t.Name(), len(d.dispatchTicks))
	}
	if t0 := d.dispatchTicks[0]; t0.Dispatched != 1 || t0.Skipped != 1 {
		t.Errorf("%s: tick record dispatched %d skipped %d, want gt-a dispatched and gt-b skipped", t.Name(), t0.Dispatched, t0.Skipped)
	}
}

// A sling that never returns is cut at the drain's bound, and the drain says so:
// the daemon does not wait out the sling's own bound (specSlingBound).
func TestShutdownDrainBoundEndsASlingThatNeverReturns(t *testing.T) {
	t.Parallel()
	b, _, d, lines := newBlockedSlings(t, plannedTick(slingJob("gt-a", "claude-sonnet")))
	d.drainBound = 10 * time.Millisecond

	d.runSpecDispatch()
	recvSling(t, b.started)
	drained := make(chan os.Signal, 1)
	go func() { drained <- d.drainForShutdown(make(chan os.Signal)) }()

	if sig := within(t, signalWait, "the drain past its bound", drained); sig != nil {
		t.Errorf("%s: the drain returned the signal %v, want nil at the bound", t.Name(), sig)
	}
	if !strings.Contains(strings.Join(lines(), "\n"), "sling still in flight after") {
		t.Errorf("%s: the drain did not report the sling it cut: %v", t.Name(), lines())
	}
	close(b.release)
	waitSlings(t, d)
}

// Once the drain stops the sling set, a tick that hands a sling back starts none,
// and the sling is reported back as not started with the shutdown as the reason.
func TestShutdownDrainRefusesASlingHandedBackAfterTheStop(t *testing.T) {
	t.Parallel()
	_, fake, d, lines := newBlockedSlings(t)
	d.drainBound = time.Hour

	if sig := d.drainForShutdown(make(chan os.Signal)); sig != nil {
		t.Fatalf("%s: the idle drain returned %v, want nil", t.Name(), sig)
	}
	d.startSpecSlings([]json.RawMessage{json.RawMessage(slingJob("gt-c", "claude-haiku"))}, 0)

	if n := len(fake.argvs("spec", "sling")); n != 0 {
		t.Errorf("%s: slings run = %d, want none once the daemon drains", t.Name(), n)
	}
	if n := countContaining(lines(), "gt-c: sling not started: the daemon is shutting down; the bead stays ready for the next daemon"); n != 1 {
		t.Errorf("%s: gt-c not-started lines = %d, want one naming the shutdown: %v", t.Name(), n, lines())
	}
}
