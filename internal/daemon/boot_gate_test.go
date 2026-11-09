package daemon

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/ring/ringtest"
)

// bootGateTestInterval is the wait between polls the tests give the gate. The
// gate runs on a fake clock, so only Advance moves it; the value just has to be
// set and short.
const bootGateTestInterval = time.Second

// bootGateOver returns a boot gate over probes that has not polled yet, with
// its wait between polls on a fake clock fixed at now. The probes keep the real
// clock for their timeout: a probe here answers at once, and a fake-clock
// timeout would also count as the poll's wait, so awaitBootPoll could return
// before the answer is stored.
func bootGateOver(t *testing.T, now time.Time, probes ...bootProbe) (*bootGate, *clockwork.FakeClock) {
	t.Helper()
	gate, err := newBootGate(probes)
	if err != nil {
		t.Fatalf("newBootGate: %v", err)
	}
	clk := clockwork.NewFakeClockAt(now)
	gate.clock = clk
	gate.interval = bootGateTestInterval
	return gate, clk
}

// startBootPoll runs the gate's poll on its own goroutine, as Run does, and
// returns the channel that closes when the poll ends. Its log lines go to the
// daemon's logger.
func startBootPoll(d *Daemon, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.bootGate.poll(ctx, d.logger.Printf)
	}()
	return done
}

// awaitBootPoll waits until the poll has stored its answer and set its wait on
// the fake clock. A poll that never gets there fails the test at the guard.
func awaitBootPoll(t *testing.T, clk *clockwork.FakeClock) {
	t.Helper()
	guard, cancel := context.WithTimeout(context.Background(), bootHangGuard) //testpolicy:allow no-sleep — a hang guard only: the poll's wait on the clock is the event the test waits for, and a poll that never sets it fails here rather than hanging the test
	defer cancel()
	if err := clk.BlockUntilContext(guard, 1); err != nil {
		t.Fatalf("the boot poll never set its wait: %v", err)
	}
}

// awaitBootPollEnd waits for the poll to end. A poll that never ends fails the
// test at the guard.
func awaitBootPollEnd(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(bootHangGuard): //testpolicy:allow no-sleep — a hang guard only: the poll's end is the event the test waits for, and a poll that never ends fails here rather than hanging the test
		t.Fatal("the boot poll never ended")
	}
}

// TestBootGateStandbyNamesTheUnanswering: with one probe down, the standby
// names it, the heartbeat runs only its non-lifecycle steps, and the tier sweep
// starts no cycle.
func TestBootGateStandbyNamesTheUnanswering(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	forgejo := &fakeProbe{}
	forgejo.up.Store(true)
	dolt := &fakeProbe{}
	gate, clk := bootGateOver(t, now, forgejo.probe("forgejo"), dolt.probe("dolt"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, ran, _ := standbyHeartbeat(t, now)
	d.bootGate = gate
	done := startBootPoll(d, ctx)
	awaitBootPoll(t, clk)

	reason := d.standbyReason(now)
	if !strings.Contains(reason, "waiting for") || !strings.Contains(reason, "dolt") {
		t.Fatalf("standbyReason = %q, want it to name waiting for dolt", reason)
	}
	d.heartbeat(&State{})
	if want := standbyStepNames(false); !slices.Equal(*ran, want) {
		t.Errorf("steps run under the boot hold = %v, want the non-lifecycle steps %v", *ran, want)
	}

	td, rec, bd, _ := newTierSweepDaemon(t, now, "gastown")
	rec.green()
	started := tierCycleStarts(td)
	td.bootGate = gate
	td.triggerTierSweep()
	waitTierSweepIdle(t, td)
	if *started != 0 {
		t.Errorf("a tier sweep cycle started under the boot hold: origin/main read %d time(s)", *started)
	}
	if len(rec.stages) != 0 || len(bd.created) != 0 {
		t.Errorf("tier sweep did work under the boot hold: stages %+v, beads created %+v", rec.stages, bd.created)
	}

	cancel()
	awaitBootPollEnd(t, done)
}

// TestBootGateBeforeTheFirstPollIsHeld: before the poll has run once the
// standby is non-empty and no probe has been asked.
func TestBootGateBeforeTheFirstPollIsHeld(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	up := &fakeProbe{}
	up.up.Store(true)
	gate, _ := bootGateOver(t, now, up.probe("dolt"))

	d, ran, _ := standbyHeartbeat(t, now)
	d.bootGate = gate
	if reason := d.standbyReason(now); reason == "" {
		t.Fatal("standbyReason before the first poll = \"\", want it held (fail closed)")
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("a probe ran before the poll started: %d call(s)", n)
	}
	d.heartbeat(&State{})
	if want := standbyStepNames(false); !slices.Equal(*ran, want) {
		t.Errorf("steps run before the first poll = %v, want the non-lifecycle steps %v", *ran, want)
	}
}

// TestBootGateLiftsWhenEveryProbeAnswersAndStaysLifted: once every probe
// answers the standby is "", the poll ends, and a probe that fails later holds
// nothing.
func TestBootGateLiftsWhenEveryProbeAnswersAndStaysLifted(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	forgejo := &fakeProbe{}
	forgejo.up.Store(true)
	dolt := &fakeProbe{}
	dolt.up.Store(true)
	gate, clk := bootGateOver(t, now, forgejo.probe("forgejo"), dolt.probe("dolt"))

	d, ran, lines := standbyHeartbeat(t, now)
	d.bootGate = gate
	done := startBootPoll(d, context.Background())
	awaitBootPollEnd(t, done)

	if reason := d.standbyReason(now); reason != "" {
		t.Fatalf("standbyReason once every probe answers = %q, want \"\"", reason)
	}
	if n := countContaining(lines(), "boot gate: infrastructure answering"); n != 1 {
		t.Errorf("the lift was logged %d time(s), want once; log: %v", n, lines())
	}

	calls := dolt.calls.Load()
	dolt.up.Store(false)
	clk.Advance(gate.interval)
	if reason := d.standbyReason(now); reason != "" {
		t.Errorf("standbyReason after a later outage = %q, want \"\" (the poll has ended)", reason)
	}
	if n := dolt.calls.Load(); n != calls {
		t.Errorf("a probe ran %d time(s) after the lift, want none", n-calls)
	}
	d.heartbeat(&State{})
	if want := allHeartbeatStepNames(); !slices.Equal(*ran, want) {
		t.Errorf("steps run after the lift = %v, want every step %v", *ran, want)
	}
}

// TestBootGateESTOPWinsOverBoot: with the boot gate unhealthy, the standby is
// the ESTOP's or the Ring's, the reason that holds first.
func TestBootGateESTOPWinsOverBoot(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	for _, tc := range []struct {
		name       string
		set        func(t *testing.T, d *Daemon)
		wantPrefix string
	}{
		{"town ESTOP", func(t *testing.T, d *Daemon) {
			if err := estop.Activate(d.config.TownRoot, estop.TriggerManual, "drill"); err != nil {
				t.Fatal(err)
			}
		}, "town ESTOP active"},
		{"waned Ring", func(t *testing.T, d *Daemon) {
			withStandbyRingKey(t, d)
			ringtest.Waned(t, d.config.TownRoot, standbyKey, now)
		}, "the Ring waned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dolt := &fakeProbe{}
			gate, clk := bootGateOver(t, now, dolt.probe("dolt"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d, _, _ := standbyHeartbeat(t, now)
			d.bootGate = gate
			done := startBootPoll(d, ctx)
			awaitBootPoll(t, clk)
			tc.set(t, d)

			reason := d.standbyReason(now)
			if !strings.HasPrefix(reason, tc.wantPrefix) {
				t.Errorf("standbyReason = %q, want the %s's reason first", reason, tc.name)
			}
			cancel()
			awaitBootPollEnd(t, done)
		})
	}
}

// TestBootGateDoesNotGateTheLandingWorkerOrDoltSupervision: under the boot hold
// the heartbeat still runs Dolt supervision, and runs no lifecycle step. The
// landing worker is its own goroutine and is not a heartbeat step, so a rig's
// worker is run directly under the same hold and must reach its rig start.
func TestBootGateDoesNotGateTheLandingWorkerOrDoltSupervision(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	dolt := &fakeProbe{}
	gate, clk := bootGateOver(t, now, dolt.probe("dolt"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, ran, lines := standbyHeartbeat(t, now)
	d.bootGate = gate
	done := startBootPoll(d, ctx)
	awaitBootPoll(t, clk)

	d.heartbeat(&State{})
	if !slices.Contains(*ran, "dolt") {
		t.Errorf("Dolt supervision did not run under the boot hold: steps run %v", *ran)
	}
	for _, s := range heartbeatSteps {
		if s.lifecycle && slices.Contains(*ran, s.name) {
			t.Errorf("lifecycle step %q ran under the boot hold", s.name)
		}
	}

	// A rig whose config does not decode is refused at its worker's start,
	// and that refusal is logged. Reaching it proves the worker ran under the
	// hold.
	const rigName = "testrig"
	rigPath := filepath.Join(d.config.TownRoot, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDaemonRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"testrig","default_branchh":"develop"}`)
	d.notifier = notifyfake.New()
	useGitfake(t, d).InitBare(t, filepath.Join(rigPath, ".repo.git"))
	if reason := d.standbyReason(now); reason == "" {
		t.Fatal("precondition: the boot hold is not in place")
	}
	d.runRigLandingWorker(&landingWorkers{running: map[string]bool{}}, rigName, time.Hour)
	if n := countContaining(lines(), "landing_worker: "+rigName+": not starting"); n != 1 {
		t.Errorf("the landing worker did not reach its rig start under the boot hold; log: %v", lines())
	}

	cancel()
	awaitBootPollEnd(t, done)
}

// TestBootGateNeverTouchesTheLease: a town with no lease file runs the whole
// sequence (the outage, the lift, the heartbeat) and no lease file appears.
// This code reads and writes no lease record; the walk checks that the
// sequence leaves none behind.
func TestBootGateNeverTouchesTheLease(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	dolt := &fakeProbe{}
	gate, clk := bootGateOver(t, now, dolt.probe("dolt"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, _, _ := standbyHeartbeat(t, now)
	d.bootGate = gate
	done := startBootPoll(d, ctx)
	awaitBootPoll(t, clk)
	d.heartbeat(&State{})
	cancel()
	awaitBootPollEnd(t, done)

	dolt.up.Store(true)
	done = startBootPoll(d, context.Background())
	awaitBootPollEnd(t, done)
	d.heartbeat(&State{})

	err := filepath.WalkDir(d.config.TownRoot, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(e.Name()), "lease") {
			t.Errorf("the boot gate sequence left a lease file behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the town: %v", err)
	}
}

// TestBootGateStopsWhenTheContextEnds: the poll ends when the daemon's context
// does, while a probe is still not answering, and asks no probe after that.
func TestBootGateStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	t.Run("ended while waiting", func(t *testing.T) {
		t.Parallel()
		dolt := &fakeProbe{}
		gate, clk := bootGateOver(t, now, dolt.probe("dolt"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		d, _, _ := standbyHeartbeat(t, now)
		d.bootGate = gate
		done := startBootPoll(d, ctx)
		awaitBootPoll(t, clk)

		cancel()
		awaitBootPollEnd(t, done)
		calls := dolt.calls.Load()
		clk.Advance(gate.interval)
		if n := dolt.calls.Load(); n != calls {
			t.Errorf("a probe ran %d time(s) after the context ended, want none", n-calls)
		}
	})
	t.Run("ended before the first poll", func(t *testing.T) {
		t.Parallel()
		dolt := &fakeProbe{}
		gate, _ := bootGateOver(t, now, dolt.probe("dolt"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		d, _, _ := standbyHeartbeat(t, now)
		d.bootGate = gate
		awaitBootPollEnd(t, startBootPoll(d, ctx))
		if n := dolt.calls.Load(); n != 0 {
			t.Errorf("a probe ran %d time(s) under a context that had already ended, want none", n)
		}
	})
}
