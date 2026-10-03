package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/version"
)

// The waits in this file carry no deadline: they block until the code under
// test signals, and a signal that never comes is a hang the package's
// go test -timeout reports. A deadline would be a wall-clock budget by another
// name, which is the defect these waits replace (gt-v0t6k).

// syncBuffer is the log sink the landing loop writes to from its own
// goroutine: a plain bytes.Buffer read while that goroutine runs is a race.
// Each write also wakes a reader, so awaitLog waits on a signal rather than on
// a runtime.Gosched budget (gt-v0t6k).
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	// One queued wake is all a reader needs: it re-reads the buffer when it
	// wakes, so a wake dropped while one is already queued loses nothing.
	if b.changed == nil {
		b.changed = make(chan struct{}, 1)
	}
	select {
	case b.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// changes returns the channel every write wakes. It carries no content of its
// own; a reader re-reads String after each wake.
func (b *syncBuffer) changes() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.changed == nil {
		b.changed = make(chan struct{}, 1)
	}
	return b.changed
}

const drainRequestLine = "rebuild_gt: install requested: queue drained after landing"

// landingDrainDaemon builds a town whose gt source checkout is the gastown
// rig's own directory, with a second rig beside it, so a test can tell a drain
// of the source rig from a drain of any other. Both rigs are operational, so
// the landing worker passes rather than skipping (gt-3qmv4.1).
func landingDrainDaemon(t *testing.T) (*Daemon, *syncBuffer) {
	t.Helper()
	townRoot := t.TempDir()
	const src, other = "gastown", "agate"
	for _, rigName := range []string{src, other} {
		rigPath := filepath.Join(townRoot, rigName)
		mkdirs(t, filepath.Join(rigPath, ".beads"))
		writeFile(t, filepath.Join(rigPath, "config.json"), fmt.Sprintf(`{"beads":{"prefix":%q}}`, rigName[:2]))
	}
	mkdirs(t, filepath.Join(townRoot, src, "cmd", "gt"))
	writeFile(t, filepath.Join(townRoot, src, "cmd", "gt", "main.go"), "package main\n")
	registerTestRigs(t, townRoot, map[string]string{src: src[:2], other: other[:2]})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logs := &syncBuffer{}
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(logs, "", 0),
		ctx:    ctx,
	}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		LandingWorker: &LandingWorkerConfig{Enabled: true},
	}}
	d.rigBeadShowFn = func(_, id string) (*beads.Issue, error) { return &beads.Issue{ID: id}, nil }
	for _, rigName := range []string{src, other} {
		if ok, why := d.isRigOperational(rigName); !ok {
			t.Fatalf("fixture rig %s is not operational (%s); the worker would skip its pass", rigName, why)
		}
	}
	return d, logs
}

// awaitLog blocks until the loop goroutine has written want, so a test reads
// the request flag only after the line that reports it landed. It waits on the
// sink's write signal, so the wait is correct at any load: the Gosched budget
// it replaces could run out before the loop goroutine was scheduled (gt-v0t6k).
func awaitLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	for !strings.Contains(logs.String(), want) {
		<-logs.changes()
	}
}

// watchRebuildGTCycle wires the cycle's end seam and returns the channel the
// cycle's goroutine signals as it ends. Wire it before triggering.
func watchRebuildGTCycle(d *Daemon) <-chan struct{} {
	cycleEnd := make(chan struct{}, 1)
	d.rebuildGTCycleDoneFn = func() {
		// One queued wake is enough: the waiter re-reads the guard after every
		// wake, so a wake dropped while one is queued loses nothing.
		select {
		case cycleEnd <- struct{}{}:
		default:
		}
	}
	return cycleEnd
}

// awaitRebuildGTIdle blocks until the cycle a trigger started has finished.
// The guard is set before the goroutine launches, so a wait that sees it clear
// saw the cycle's own store. It wakes on the cycle's end signal rather than
// spending a runtime.Gosched budget, which under load runs out before the
// cycle goroutine is scheduled — TestRebuildGTRequestedInstallRetriesPastThe
// Interval failed 1 run in 40 that way (gt-v0t6k).
func awaitRebuildGTIdle(t *testing.T, d *Daemon, cycleEnd <-chan struct{}) {
	t.Helper()
	for d.rebuildGTRunning.Load() {
		<-cycleEnd
	}
}

// passDone counts the landing passes the loop has finished and wakes a waiter
// on each one. The loop calls done through d.landingPassFn once a pass's report
// has been applied; a test whose pass function switches on how many passes have
// run still counts the passes it starts, which is a different event.
type passDone struct {
	n    atomic.Int32
	wake chan struct{}
}

// countPasses wires the loop's pass seam to a counter a test can wait on.
// Wire it before starting the loop.
func countPasses(d *Daemon) *passDone {
	p := &passDone{wake: make(chan struct{}, 1)}
	d.landingPassFn = p.done
	return p
}

// done records a finished pass and wakes the waiter. It runs on the landing
// loop's goroutine, so it never blocks: the wake is a nudge the waiter
// re-reads the count against, and one dropped while a nudge is already queued
// still leaves it reading a count that moved on.
func (p *passDone) done() {
	p.n.Add(1)
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// settlePasses blocks until the loop has finished want passes, so a negative
// assertion is made after the pass that would have requested an install. It
// wakes on the loop's own pass signal: the Gosched budget it replaces could
// run out before the loop goroutine was scheduled, which failed
// TestLandingInstallNowOnAnotherRigRequestsNoInstall 8 runs in 40 with
// "passes = 1, want at least 3" (gt-v0t6k).
func settlePasses(t *testing.T, passes *passDone, want int32) {
	t.Helper()
	for passes.n.Load() < want {
		<-passes.wake
	}
}

// TestLandingDrainRequestsAnInstall pins the drain signal: a pass that landed
// and the idle pass right after it say main just moved under the installed gt
// binary, so the next rebuild_gt heartbeat must install rather than hold the
// hour it would otherwise wait (gt-3qmv4.1).
func TestLandingDrainRequestsAnInstall(t *testing.T) {
	t.Parallel()
	d, logs := landingDrainDaemon(t)
	var passes atomic.Int32
	pass := func(context.Context) landworker.Report {
		if passes.Add(1) == 1 {
			return landworker.Report{Landed: 1}
		}
		return landworker.Report{}
	}
	// The interval is an hour: only the drain arms the request, and an idle
	// pass after it waits, so the loop makes no further request.
	go d.landingWorkerLoop("gastown", time.Hour, pass)

	awaitLog(t, logs, drainRequestLine)
	if !d.rebuildGTRequested.Load() {
		t.Fatal("the drain logged a request it did not arm")
	}
	if n := strings.Count(logs.String(), drainRequestLine); n != 1 {
		t.Fatalf("request lines = %d, want 1:\n%s", n, logs.String())
	}
}

// TestLandingDrainSurvivesANonLandingPass pins the drain across a pass that
// did not land: a rejection sends its bead to rework, so it is not the queue
// emptying, and the idle pass after it is still the quiet point (gt-3qmv4.4).
func TestLandingDrainSurvivesANonLandingPass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// A rejection pass spins straight into the next pass, a skipped one
		// waits out the interval, so only the skip needs a short one.
		interval time.Duration
		mid      landworker.Report
	}{
		{name: "rejected", interval: time.Hour, mid: landworker.Report{Rejected: 1}},
		{name: "skipped", interval: time.Nanosecond, mid: landworker.Report{Skipped: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, logs := landingDrainDaemon(t)
			var passes atomic.Int32
			pass := func(context.Context) landworker.Report {
				switch passes.Add(1) {
				case 1:
					return landworker.Report{Landed: 1}
				case 2:
					return tc.mid
				}
				return landworker.Report{}
			}
			go d.landingWorkerLoop("gastown", tc.interval, pass)

			awaitLog(t, logs, drainRequestLine)
			if !d.rebuildGTRequested.Load() {
				t.Fatal("a pass that did not land dropped the install request")
			}
			if n := strings.Count(logs.String(), drainRequestLine); n != 1 {
				t.Fatalf("request lines = %d, want 1:\n%s", n, logs.String())
			}
		})
	}
}

// TestLandingRejectWithWorkQueuedNeverRequestsAnInstall pins that a rejection
// is not itself the drain: while the queue still holds work the pass after the
// rejection lands rather than finds nothing, so the install waits for the pass
// that actually empties it (gt-3qmv4.4).
func TestLandingRejectWithWorkQueuedNeverRequestsAnInstall(t *testing.T) {
	t.Parallel()
	d, logs := landingDrainDaemon(t)
	var passes atomic.Int32
	pass := func(ctx context.Context) landworker.Report {
		switch passes.Add(1) {
		case 1, 3:
			return landworker.Report{Landed: 1}
		case 2:
			return landworker.Report{Rejected: 1}
		}
		// The queue never empties: hold the pass the loop would drain on.
		<-ctx.Done()
		return landworker.Report{}
	}
	done := countPasses(d)
	go d.landingWorkerLoop("gastown", time.Hour, pass)

	// Three finished passes: the loop held the fourth, the one it would have
	// drained on, so no idle pass has drained the queue.
	settlePasses(t, done, 3)
	if d.rebuildGTRequested.Load() {
		t.Fatal("a rejection with work still queued requested an install")
	}
	if strings.Contains(logs.String(), drainRequestLine) {
		t.Fatalf("the queue never emptied, yet a drain was logged:\n%s", logs.String())
	}
}

// TestLandingDrainOnAnotherRigNeverRequestsAnInstall pins the rig gate: a
// landing on any other rig moves that rig's main, not the main the binary is
// built from.
func TestLandingDrainOnAnotherRigNeverRequestsAnInstall(t *testing.T) {
	t.Parallel()
	d, logs := landingDrainDaemon(t)
	var passes atomic.Int32
	pass := func(context.Context) landworker.Report {
		if passes.Add(1) == 1 {
			return landworker.Report{Landed: 1}
		}
		return landworker.Report{}
	}
	done := countPasses(d)
	go d.landingWorkerLoop("agate", time.Hour, pass)

	// Two finished passes, so the idle second pass has been through the drain
	// check: the rig gate, not an unfinished pass, is what held the request.
	settlePasses(t, done, 2)
	if d.rebuildGTRequested.Load() {
		t.Fatal("a landing on another rig requested an install")
	}
	if strings.Contains(logs.String(), drainRequestLine) {
		t.Fatalf("a landing on another rig logged a request:\n%s", logs.String())
	}
}

// TestLandingIdleAfterIdleNeverRequestsAnInstall pins the other half of the
// signal: a worker whose queue was already empty says nothing about main, so
// idle passes alone must not install.
func TestLandingIdleAfterIdleNeverRequestsAnInstall(t *testing.T) {
	t.Parallel()
	d, logs := landingDrainDaemon(t)
	done := countPasses(d)
	pass := func(context.Context) landworker.Report { return landworker.Report{} }
	// A nanosecond interval lets the idle passes recur; the default hour would
	// end the test after one.
	go d.landingWorkerLoop("gastown", time.Nanosecond, pass)

	settlePasses(t, done, 3)
	if d.rebuildGTRequested.Load() {
		t.Fatal("an idle pass after an idle pass requested an install")
	}
	if strings.Contains(logs.String(), drainRequestLine) {
		t.Fatalf("idle passes logged a request:\n%s", logs.String())
	}
}

// TestLandingOwnsGTSourceOnlyMatchesTheSourceRig pins the rig lookup both
// landing loop tests lean on: the source checkout is inside the gastown rig,
// and nothing about another rig's path is inside it.
func TestLandingOwnsGTSourceOnlyMatchesTheSourceRig(t *testing.T) {
	t.Parallel()
	d, _ := landingDrainDaemon(t)
	if !d.landingOwnsGTSource("gastown") {
		t.Error("the gastown rig's directory is the town's gt source")
	}
	if d.landingOwnsGTSource("agate") {
		t.Error("no checkout under the agate rig is the town's gt source")
	}
}

// TestRebuildGTInstallRequestIsArmedOnce pins the request's idempotence: a
// second drain while one is pending adds no second line, so the log counts
// drains rather than heartbeats (gt-3qmv4.1).
func TestRebuildGTInstallRequestIsArmedOnce(t *testing.T) {
	t.Parallel()
	d, logs := landingDrainDaemon(t)
	d.requestRebuildGTInstall()
	d.requestRebuildGTInstall()
	if !d.rebuildGTRequested.Load() {
		t.Fatal("the request was not armed")
	}
	if n := strings.Count(logs.String(), drainRequestLine); n != 1 {
		t.Fatalf("request lines = %d, want 1:\n%s", n, logs.String())
	}
}

// TestRebuildGTRequestedInstallRetriesPastTheInterval pins the request's
// contract: it makes the job due while the patrol interval has not elapsed, it
// survives a not-quiet deferral, and only a cycle that reached a verdict
// clears it (gt-3qmv4.1).
func TestRebuildGTRequestedInstallRetriesPastTheInterval(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	var busy atomic.Bool
	busy.Store(true)
	d.rebuildGTGateFn = func() (string, bool) {
		if busy.Load() {
			return "a gate suite holds a slot (gastown/landing)", true
		}
		return "", false
	}
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case c.name == "bash":
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	// A cycle reached its verdict just now: the interval has not elapsed, so
	// only the drain request can make the job run.
	if err := savePatrolLastRun(d.config.TownRoot, "rebuild_gt", d.clk().Now()); err != nil {
		t.Fatal(err)
	}
	d.requestRebuildGTInstall()
	cycleEnd := watchRebuildGTCycle(d)

	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, cycleEnd)
	if !d.rebuildGTRequested.Load() {
		t.Fatal("a not-quiet deferral dropped the install request")
	}
	if got := installCalls(cli); len(got) != 0 {
		t.Fatalf("built while a gate held the slot: %v", got)
	}

	busy.Store(false)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, cycleEnd)
	if d.rebuildGTRequested.Load() {
		t.Fatal("a cycle that reached a verdict left the request armed")
	}
	if got := installCalls(cli); len(got) != 1 {
		t.Fatalf("installs = %v, want the one the surviving request ran", got)
	}
}
