package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/version"
)

// syncBuffer is the log sink the landing loop writes to from its own
// goroutine: a plain bytes.Buffer read while that goroutine runs is a race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

// awaitLog spins until the loop goroutine has written want, so a test reads
// the request flag only after the line that reports it landed.
func awaitLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	for i := 0; i < 10_000_000 && !strings.Contains(logs.String(), want); i++ {
		runtime.Gosched()
	}
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log never carried %q:\n%s", want, logs.String())
	}
}

// awaitRebuildGTIdle blocks until the cycle a trigger started has finished.
// The guard is set before the goroutine launches, so a spin that sees it clear
// saw the cycle's own store.
func awaitRebuildGTIdle(t *testing.T, d *Daemon) {
	t.Helper()
	for i := 0; i < 10_000_000 && d.rebuildGTRunning.Load(); i++ {
		runtime.Gosched()
	}
	if d.rebuildGTRunning.Load() {
		t.Fatal("the rebuild_gt cycle a trigger started did not finish")
	}
}

// settlePasses spins until the loop has run want passes, so a negative
// assertion is made after the pass that would have requested an install.
func settlePasses(t *testing.T, passes *atomic.Int32, want int32) {
	t.Helper()
	for i := 0; i < 10_000_000 && passes.Load() < want; i++ {
		runtime.Gosched()
	}
	if passes.Load() < want {
		t.Fatalf("passes = %d, want at least %d", passes.Load(), want)
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
	go d.landingWorkerLoop("agate", time.Hour, pass)

	settlePasses(t, &passes, 2)
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
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
	var passes atomic.Int32
	pass := func(context.Context) landworker.Report {
		passes.Add(1)
		return landworker.Report{}
	}
	// A nanosecond interval lets the idle passes recur; the default hour would
	// end the test after one.
	go d.landingWorkerLoop("gastown", time.Nanosecond, pass)

	settlePasses(t, &passes, 3)
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

	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d)
	if !d.rebuildGTRequested.Load() {
		t.Fatal("a not-quiet deferral dropped the install request")
	}
	if got := installCalls(cli); len(got) != 0 {
		t.Fatalf("built while a gate held the slot: %v", got)
	}

	busy.Store(false)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d)
	if d.rebuildGTRequested.Load() {
		t.Fatal("a cycle that reached a verdict left the request armed")
	}
	if got := installCalls(cli); len(got) != 1 {
		t.Fatalf("installs = %v, want the one the surviving request ran", got)
	}
}
