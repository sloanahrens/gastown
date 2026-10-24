package daemon

import (
	"context"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// Background slings (gt-5h70s): a tick plans and returns, its slings run on in
// the daemon, and the next tick does not wait for them.

// slingJob is the tick report's job object for one planned sling.
func slingJob(bead, agent string) string {
	return `{"bead":"` + bead + `","rig":"gastown","agent":"` + agent + `","paths":["internal/x.go"],"run":{"Formula":"","Vars":null,"Actor":""}}`
}

// plannedTick is the report of a tick that planned the jobs.
func plannedTick(jobs ...string) string {
	return `{"candidates": 2, "pending": [` + strings.Join(jobs, ",") + `]}`
}

// slingWait bounds every wait of these tests on a sling: a sling that never
// starts or never returns fails the one test that waits, not the gate. Nothing
// is timed against it; the tests move the fake clock themselves.
const slingWait = 30 * time.Second

// slingClock is the daemon's clock in these tests: a fake that a sling moves by
// slingTook while it runs, as a real sling takes time.
const slingTook = 90 * time.Second

func newSlingClock() *clockwork.FakeClock {
	return clockwork.NewFakeClockAt(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
}

// recvSling waits, bounded, for the next sling a blockedSlings announces.
func recvSling(t *testing.T, started <-chan string) string {
	ctx, cancel := context.WithTimeout(t.Context(), slingWait)
	defer cancel()
	t.Helper()
	select {
	case job := <-started:
		return job
	case <-ctx.Done():
		t.Fatalf("no sling started within %s", slingWait)
		return ""
	}
}

// waitSlings waits, bounded, for the daemon's slings to return.
func waitSlings(t *testing.T, d *Daemon) {
	ctx, cancel := context.WithTimeout(t.Context(), slingWait)
	defer cancel()
	t.Helper()
	done := make(chan struct{})
	go func() {
		d.specDispatchCycles.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("slings still in flight after %s: %+v", slingWait, d.specSlings.list())
	}
}

// blockedSlings answers a daemon's gt calls: every tick with the next report in
// ticks, every `gt spec sling` once release is closed, announcing each sling on
// started first. A test thereby holds a sling in flight without a timer. The
// clock moves by slingTook inside each sling.
type blockedSlings struct {
	ticks   []string
	release chan struct{}
	started chan string
	clock   *clockwork.FakeClock
	tick    int
}

func (b *blockedSlings) answer(args []string) cliReply {
	if len(args) >= 2 && args[0] == "spec" && args[1] == "sling" {
		b.started <- args[len(args)-1]
		<-b.release
		b.clock.Advance(slingTook)
		return cliReply{stdout: string(wireOut(`{"dispatched":[{"bead":"gt-a","line":"gt-a: slung to gastown/p on claude-sonnet"}]}`))}
	}
	report := b.ticks[min(b.tick, len(b.ticks)-1)]
	b.tick++
	return cliReply{stdout: string(wireOut(report))}
}

// newBlockedSlings is a blockedSlings and the daemon that runs against it, on
// the fake clock.
func newBlockedSlings(t *testing.T, ticks ...string) (*blockedSlings, *fakeCLI, *Daemon, func() []string) {
	t.Helper()
	b := &blockedSlings{
		ticks:   ticks,
		release: make(chan struct{}),
		started: make(chan string, 4),
		clock:   newSlingClock(),
	}
	fake := newFakeCLI(b.answer)
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = b.clock
	return b, fake, d, lines
}

func TestNextTickStartsWhileAnEarlierSlingIsStillRunning(t *testing.T) {
	t.Parallel()
	b, fake, d, lines := newBlockedSlings(t,
		plannedTick(slingJob("gt-a", "claude-sonnet")), plannedTick(slingJob("gt-b", "deepseek-flash")))

	d.runSpecDispatch()
	if got := recvSling(t, b.started); !strings.Contains(got, `"bead":"gt-a"`) {
		t.Fatalf("first sling job = %s, want gt-a's job as the tick printed it", got)
	}
	// gt-a's sling is blocked. The tick returned without it, and the next tick
	// runs now.
	if running := d.specSlings.list(); len(running) != 1 || running[0].Bead != "gt-a" {
		t.Fatalf("in flight = %+v, want gt-a", running)
	}
	d.runSpecDispatch()
	if got := recvSling(t, b.started); !strings.Contains(got, `"bead":"gt-b"`) {
		t.Fatalf("second sling job = %s, want gt-b's: the next tick must sling the other candidate", got)
	}

	// The second tick was told gt-a is running, with its seat and files.
	ticks := fake.argvs("spec", "dispatch")
	if len(ticks) != 2 {
		t.Fatalf("ticks = %v, want two", ticks)
	}
	if !slices.Contains(ticks[0], "--background-slings") || slices.Contains(ticks[0], "--in-flight") {
		t.Errorf("first tick argv = %v, want background slings and nothing in flight", ticks[0])
	}
	i := slices.Index(ticks[1], "--in-flight")
	if i < 0 || !strings.Contains(ticks[1][i+1], `"bead":"gt-a"`) || !strings.Contains(ticks[1][i+1], `"agent":"claude-sonnet"`) || !strings.Contains(ticks[1][i+1], "internal/x.go") {
		t.Errorf("second tick argv = %v, want --in-flight naming gt-a, its seat and its files", ticks[1])
	}

	close(b.release)
	waitSlings(t, d)
	if running := d.specSlings.list(); len(running) != 0 {
		t.Fatalf("in flight after the slings returned = %+v, want none", running)
	}
	got := lines()
	// The fake answers both slings with the same report.
	if n := countContaining(got, "spec_dispatch: dispatched: gt-a: slung to gastown/p", "sling took"); n != 2 {
		t.Errorf("sling result lines = %d, want one per finished sling: %v", n, got)
	}
}

func TestARunningBeadIsNeverSlungTwice(t *testing.T) {
	t.Parallel()
	// Both ticks hand back gt-a: the second tick's board was read before the
	// first sling hooked it.
	b, fake, d, lines := newBlockedSlings(t, plannedTick(slingJob("gt-a", "claude-sonnet")))

	d.runSpecDispatch()
	recvSling(t, b.started)
	d.runSpecDispatch()

	if got := fake.argvs("spec", "sling"); len(got) != 1 {
		t.Fatalf("slings run = %v, want gt-a slung once while one is in flight", got)
	}
	if n := countContaining(lines(), "gt-a: sling not started: one is already running for it"); n != 1 {
		t.Errorf("duplicate lines = %d, want the refused second sling logged once: %v", n, lines())
	}
	close(b.release)
	waitSlings(t, d)
}

func TestAFailedBackgroundSlingIsLoggedLikeAForegroundFailure(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func(args []string) cliReply {
		if args[1] == "sling" {
			return cliReply{stdout: string(wireOut(`{"failed":[{"bead":"gt-z","line":"gt-z: dispatch failed, left unassigned: boom; excluded from the queue until label spec-dispatch-failed is cleared"}]}`))}
		}
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = newSlingClock()

	d.runSpecDispatch()
	waitSlings(t, d)

	if n := countContaining(lines(), "spec_dispatch: failed: gt-z", "dispatch failed, left unassigned: boom"); n != 1 {
		t.Errorf("failed lines = %d, want one naming the bead and the reason: %v", n, lines())
	}
	if running := d.specSlings.list(); len(running) != 0 {
		t.Errorf("in flight = %+v, want a finished sling out of the set", running)
	}
	// `gt spec sling` labeled the bead itself; the daemon writes nothing more.
	if got := fake.argvs("bead"); len(got) != 0 {
		t.Errorf("daemon bead writes = %v, want none for a sling that reported its failure", got)
	}
}

// A sling whose process is killed at its bound or dies with no report never
// reached its own label and note, so the daemon labels the bead as a failed
// foreground sling would be.
func TestADeadBackgroundSlingProcessLabelsItsBead(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func(args []string) cliReply {
		if args[1] == "sling" {
			return cliReply{stderr: "signal: killed", code: 1}
		}
		if args[0] == "bead" {
			return cliReply{}
		}
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = newSlingClock()

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("bead", "update"); len(got) != 1 || !slices.Equal(got[0], []string{"bead", "update", "gt-z", "--add-label", "spec-dispatch-failed"}) {
		t.Errorf("label calls = %v, want gt-z labeled spec-dispatch-failed once", got)
	}
	notes := fake.argvs("bead", "note", "gt-z")
	if len(notes) != 1 || !strings.Contains(notes[0][len(notes[0])-1], "sling process failed") || !strings.Contains(notes[0][len(notes[0])-1], "spec-dispatch-failed is cleared") {
		t.Errorf("note calls = %v, want one note saying the sling process failed and how to clear the label", notes)
	}
	if n := countContaining(lines(), "spec_dispatch: gt-z: sling process failed"); n != 1 {
		t.Errorf("failure lines = %d, want one: %v", n, lines())
	}
	if running := d.specSlings.list(); len(running) != 0 {
		t.Errorf("in flight = %+v, want a dead sling out of the set", running)
	}
}

// A sling cut because the daemon is shutting down failed through no fault of
// its bead, which stays ready for the next daemon.
func TestAShutdownCutSlingDoesNotLabelItsBead(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithCancel(context.Background())
	fake := newFakeCLI(func(args []string) cliReply {
		if args[1] == "sling" {
			stop() // the daemon shuts down while the sling runs
			return cliReply{stderr: "signal: killed", code: 1}
		}
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, _ := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.ctx = ctx
	d.clock = newSlingClock()

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("bead"); len(got) != 0 {
		t.Errorf("bead writes = %v, want none for a sling the shutdown cut", got)
	}
}

// With a failed bead-write the failure is logged and the set still empties.
func TestAFailedFailureWriteIsLoggedNotFatal(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func(args []string) cliReply {
		switch args[0] {
		case "bead":
			return cliReply{stderr: "dolt down", code: 1}
		case "spec":
			if args[1] == "sling" {
				return cliReply{code: 1}
			}
		}
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = newSlingClock()

	d.runSpecDispatch()
	waitSlings(t, d)

	if n := countContaining(lines(), "spec_dispatch: gt-z: gt update failed", "dolt down"); n != 1 {
		t.Errorf("label failure lines = %d, want one: %v", n, lines())
	}
	if n := countContaining(lines(), "spec_dispatch: gt-z: gt note failed"); n != 1 {
		t.Errorf("note failure lines = %d, want one: %v", n, lines())
	}
}

// A running sling holds the restart; so does a patrol scan in flight with it.
// The scan's hold must not be masked by the sling's, whose own bound is the
// sling case's, not the housekeeping cap's.
func TestDaemonWorkHoldNamesOtherSoftWorkAheadOfARunningSling(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", started: time.Now()})
	if hold := d.daemonWorkHold(); hold == nil || hold.name != specSlingHoldLabel || !hold.ownBound {
		t.Fatalf("hold = %+v, want the sling hold when nothing else runs", hold)
	}
	d.patrolScanRunning.Store(true)
	if hold := d.daemonWorkHold(); hold == nil || hold.name != "a patrol scan" || hold.ownBound {
		t.Fatalf("hold = %+v, want the patrol scan named ahead of the sling", hold)
	}
}

func TestNoSlingStartsWhileAnUpgradeRestartIsPending(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func([]string) cliReply {
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-a", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.upgradeRestartPending.Store(true)

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("spec", "sling"); len(got) != 0 {
		t.Fatalf("slings run = %v, want none under a pending restart", got)
	}
	if n := countContaining(lines(), "gt-a: sling not started: an upgrade restart is pending"); n != 1 {
		t.Errorf("lines = %v, want the held sling named", lines())
	}
}

// The restart waits for the in-flight slings, bounded by the sling's own bound
// from the oldest one's start, not by the 2m housekeeping cap.
func TestUpgradeWaitsForARunningSlingToItsBound(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	bound := specSlingBound()
	if bound <= housekeepingRestartCap {
		t.Fatalf("specSlingBound = %s, want it past the housekeeping cap %s", bound, housekeepingRestartCap)
	}
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", Rig: "gastown", Agent: "claude-sonnet", started: now})

	if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(housekeepingRestartCap)) || d.checkUpgradeRestart(now.Add(bound-time.Second)) {
		t.Fatal("restarted under a running sling inside its bound")
	}
	if !strings.Contains(logs.String(), "waiting for "+specSlingHoldLabel) {
		t.Fatalf("the wait did not name the sling:\n%s", logs.String())
	}
	if !d.checkUpgradeRestart(now.Add(bound)) {
		t.Fatal("the restart must go once the sling is past its bound")
	}
	if !strings.Contains(logs.String(), specSlingHoldLabel+" is still in flight after its") {
		t.Fatalf("no cut-short line naming the sling:\n%s", logs.String())
	}
}

func TestUpgradeRestartGoesWhenTheRunningSlingsReturn(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", started: now})
	d.specSlings.begin(specSlingFlight{Bead: "gt-b", started: now})
	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under running slings")
	}
	d.specSlings.end("gt-a")
	if d.checkUpgradeRestart(now.Add(time.Minute)) {
		t.Fatal("restarted while one sling still runs")
	}
	d.specSlings.end("gt-b")
	if !d.checkUpgradeRestart(now.Add(2 * time.Minute)) {
		t.Fatal("the restart must go as soon as the in-flight set is empty")
	}
}
