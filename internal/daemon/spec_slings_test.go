package daemon

import (
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

// blockedSlings answers a daemon's gt calls: every tick with the next report in
// ticks, every `gt spec sling` once release is closed, announcing each sling on
// started first. A test thereby holds a sling in flight without a timer.
type blockedSlings struct {
	ticks   []string
	release chan struct{}
	started chan string
	tick    int
}

func (b *blockedSlings) answer(args []string) cliReply {
	if len(args) >= 2 && args[0] == "spec" && args[1] == "sling" {
		b.started <- args[len(args)-1]
		<-b.release
		return cliReply{stdout: string(wireOut(`{"dispatched":[{"bead":"gt-a","line":"gt-a: slung to gastown/p on claude-sonnet"}]}`))}
	}
	report := b.ticks[min(b.tick, len(b.ticks)-1)]
	b.tick++
	return cliReply{stdout: string(wireOut(report))}
}

func TestNextTickStartsWhileAnEarlierSlingIsStillRunning(t *testing.T) {
	t.Parallel()
	b := &blockedSlings{
		ticks:   []string{plannedTick(slingJob("gt-a", "claude-sonnet")), plannedTick(slingJob("gt-b", "deepseek-flash"))},
		release: make(chan struct{}),
		started: make(chan string, 4),
	}
	fake := newFakeCLI(b.answer)
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = clockwork.NewFakeClockAt(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))

	d.runSpecDispatch()
	if got := <-b.started; !strings.Contains(got, `"bead":"gt-a"`) {
		t.Fatalf("first sling job = %s, want gt-a's job as the tick printed it", got)
	}
	// gt-a's sling is blocked. The tick returned without it, and the next tick
	// runs now.
	if running := d.specSlings.list(); len(running) != 1 || running[0].Bead != "gt-a" {
		t.Fatalf("in flight = %+v, want gt-a", running)
	}
	d.runSpecDispatch()
	if got := <-b.started; !strings.Contains(got, `"bead":"gt-b"`) {
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
	d.specDispatchCycles.Wait()
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
	b := &blockedSlings{
		// Both ticks hand back gt-a: the second tick's board was read before the
		// first sling hooked it.
		ticks:   []string{plannedTick(slingJob("gt-a", "claude-sonnet"))},
		release: make(chan struct{}),
		started: make(chan string, 4),
	}
	fake := newFakeCLI(b.answer)
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)

	d.runSpecDispatch()
	<-b.started
	d.runSpecDispatch()

	if got := fake.argvs("spec", "sling"); len(got) != 1 {
		t.Fatalf("slings run = %v, want gt-a slung once while one is in flight", got)
	}
	if n := countContaining(lines(), "gt-a: sling not started: one is already running for it"); n != 1 {
		t.Errorf("duplicate lines = %d, want the refused second sling logged once: %v", n, lines())
	}
	close(b.release)
	d.specDispatchCycles.Wait()
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

	d.runSpecDispatch()
	d.specDispatchCycles.Wait()

	if n := countContaining(lines(), "spec_dispatch: failed: gt-z", "dispatch failed, left unassigned: boom"); n != 1 {
		t.Errorf("failed lines = %d, want one naming the bead and the reason: %v", n, lines())
	}
	if running := d.specSlings.list(); len(running) != 0 {
		t.Errorf("in flight = %+v, want a finished sling out of the set", running)
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
	d.specDispatchCycles.Wait()

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
