package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestNewDashboardMux_PartitionsSubprocessPools is the regression test for
// the major finding on the first gt-d5xr fix: sharing one pool between the
// fetcher and APIHandler meant four slow /api/run children could hold every
// slot and starve the dashboard render. NewDashboardMux must NOT share the
// pools; the long-running user commands must draw from a pool of their own
// (userCommandConcurrency) that the fetcher's bd reads never touch.
func TestNewDashboardMux_PartitionsSubprocessPools(t *testing.T) {
	fetcher := &LiveConvoyFetcher{cmdSem: make(chan struct{}, subprocessConcurrency)}

	mux, err := NewDashboardMux(fetcher, nil)
	if err != nil {
		t.Fatalf("NewDashboardMux: %v", err)
	}
	serveMux, ok := mux.(*http.ServeMux)
	if !ok {
		t.Fatalf("NewDashboardMux returned %T, want *http.ServeMux", mux)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/commands", nil)
	h, pattern := serveMux.Handler(req)
	if pattern == "" {
		t.Fatal("no handler registered for /api/commands")
	}
	apiHandler, ok := h.(*APIHandler)
	if !ok {
		t.Fatalf("/api/ handler is %T, want *APIHandler", h)
	}

	// The pools are deliberately not shared: sharing the short-read pool
	// with the long-running user commands lets four slow /api/runs hold
	// every slot and starve the render (gt-d5xr).
	if apiHandler.cmdSem == fetcher.cmdSem { //nolint:staticcheck // intentional channel identity check
		t.Fatal("apiHandler.cmdSem is the fetcher's pool — long-running user commands can starve the bd reads")
	}
	if apiHandler.userCmdSem == nil {
		t.Fatal("apiHandler.userCmdSem is nil — user-driven runs are unbounded")
	}
	if cap(apiHandler.userCmdSem) != userCommandConcurrency {
		t.Fatalf("apiHandler.userCmdSem capacity = %d, want %d", cap(apiHandler.userCmdSem), userCommandConcurrency)
	}
	// The user pool must not be the fetcher's bd-read pool either: the
	// background polecat refresh draws user slots, not read slots.
	if apiHandler.userCmdSem == fetcher.cmdSem { //nolint:staticcheck // intentional channel identity check
		t.Fatal("apiHandler.userCmdSem is the fetcher's bd pool — the refresh would compete with the reads")
	}
}

// fakeProcs is a procRunner double: it answers every child in-process and
// records how many run at once. A child that finds gate non-nil reports on
// entered and then blocks until gate closes, so a test holds a known number
// of children in flight instead of hoping a loaded host forks them in time.
type fakeProcs struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	calls    int
	argv     [][]string

	entered chan struct{} // buffered by the test; one send per gated child
	gate    chan struct{} // nil: children return at once

	// onRun, if set, runs inside the child (after the in-flight count is
	// taken) and decides its outcome; nil writes "[]" to stdout.
	onRun func(ctx context.Context, cmd *exec.Cmd) error
}

func (p *fakeProcs) run(ctx context.Context, cmd *exec.Cmd) error {
	p.mu.Lock()
	p.inFlight++
	p.calls++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	p.argv = append(p.argv, append([]string(nil), cmd.Args...))
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()

	if p.gate != nil {
		p.entered <- struct{}{}
		<-p.gate
	}
	if p.onRun != nil {
		return p.onRun(ctx, cmd)
	}
	_, err := io.WriteString(cmd.Stdout, "[]")
	return err
}

func (p *fakeProcs) stats() (peak, calls int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak, p.calls
}

// newGatedProcs returns a fakeProcs whose children all block until release
// is called, with room on entered for n of them to report in.
func newGatedProcs(n int) (p *fakeProcs, release func()) {
	p = &fakeProcs{entered: make(chan struct{}, n), gate: make(chan struct{})}
	return p, func() { close(p.gate) }
}

// awaitEntered receives n child-entered reports.
func awaitEntered(t *testing.T, p *fakeProcs, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		<-p.entered
	}
}

// poolOccupancy records, from inside a fake child, how many slots each of an
// APIHandler's pools holds while that child runs. It is how the tests below
// prove which pool a command drew from without letting a wrong draw hang.
type poolOccupancy struct {
	cmdSem, userCmdSem int
}

func occupancyProbe(h *APIHandler, seen *poolOccupancy, out string) procRunner {
	return func(_ context.Context, cmd *exec.Cmd) error {
		*seen = poolOccupancy{cmdSem: len(h.cmdSem), userCmdSem: len(h.userCmdSem)}
		_, err := io.WriteString(cmd.Stdout, out)
		return err
	}
}

// TestUserCommandPoolDoesNotStarveFetcherBD is the behavioral proof of the
// partition, driving both handlers it claims are independent. The fetcher's
// bd-read pool is held completely full by in-flight reads, as fetchAndRender's
// herd leaves it; a user-driven command must still run, because it draws from
// the APIHandler's userCmdSem only (see runUserGtCommand), and the held reads
// must all complete once they are released, never more than the bound at once
// (gt-d5xr). Every child is a fake and the clock never moves, so nothing here
// can time out: the outcome depends only on which pool each call draws.
func TestUserCommandPoolDoesNotStarveFetcherBD(t *testing.T) {
	const numCalls = 17

	bdProcs, releaseBD := newGatedProcs(numCalls)
	f := &LiveConvoyFetcher{
		cmdTimeout: 30 * time.Second,
		cmdSem:     make(chan struct{}, subprocessConcurrency),
		clock:      clockwork.NewFakeClock(),
		runProc:    bdProcs.run,
	}

	h := &APIHandler{
		cmdSem:     make(chan struct{}, maxConcurrentCommands),
		userCmdSem: make(chan struct{}, userCommandConcurrency),
		clock:      clockwork.NewFakeClock(),
	}
	var seen poolOccupancy
	h.runProc = occupancyProbe(h, &seen, "ok")

	var wg sync.WaitGroup
	var failures atomic.Int64
	wg.Add(numCalls)
	for i := 0; i < numCalls; i++ {
		go func() {
			defer wg.Done()
			if _, err := f.runBdCmd(t.TempDir(), "show", "gt-d5xr"); err != nil {
				failures.Add(1)
				t.Errorf("runBdCmd: %v", err)
			}
		}()
	}
	// The fetcher's pool is now full of held reads; the rest queue behind it.
	awaitEntered(t, bdProcs, subprocessConcurrency)
	if got := len(f.cmdSem); got != subprocessConcurrency {
		t.Fatalf("fetcher cmdSem holds %d slots with %d reads in flight, want it full (%d)", got, subprocessConcurrency, subprocessConcurrency)
	}

	out, err := h.runUserGtCommand(context.Background(), time.Minute, []string{"rig", "add"})
	if err != nil {
		t.Fatalf("runUserGtCommand with the fetcher's bd pool full: %v", err)
	}
	if out != "ok" {
		t.Fatalf("runUserGtCommand output = %q, want %q", out, "ok")
	}
	if seen != (poolOccupancy{cmdSem: 0, userCmdSem: 1}) {
		t.Fatalf("while the user command ran, APIHandler pools held %+v, want only its own userCmdSem slot {cmdSem:0 userCmdSem:1}", seen)
	}

	releaseBD()
	wg.Wait()

	peak, calls := bdProcs.stats()
	if calls != numCalls || failures.Load() != 0 {
		t.Fatalf("bd reads: %d ran, %d failed, want all %d to run and none to fail", calls, failures.Load(), numCalls)
	}
	if peak != subprocessConcurrency {
		t.Fatalf("peak concurrent bd children = %d, want exactly the bound %d", peak, subprocessConcurrency)
	}
}

// TestRunUserGtCommand_DoesNotDrawCmdSemSlot is the minimal regression for
// runUserGtCommand once wrapping runGtCommand, which also acquired cmdSem,
// so a long user run held a short-read slot for its whole lifetime (gt-d5xr).
// The child reports which pools are held while it runs.
func TestRunUserGtCommand_DoesNotDrawCmdSemSlot(t *testing.T) {
	h := &APIHandler{
		cmdSem:     make(chan struct{}, 1),
		userCmdSem: make(chan struct{}, 1),
		clock:      clockwork.NewFakeClock(),
	}
	var seen poolOccupancy
	h.runProc = occupancyProbe(h, &seen, "ok")

	out, err := h.runUserGtCommand(context.Background(), time.Minute, []string{"rig", "add"})
	if err != nil {
		t.Fatalf("runUserGtCommand: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want %q", out, "ok")
	}
	if seen != (poolOccupancy{cmdSem: 0, userCmdSem: 1}) {
		t.Fatalf("pools held while the user command ran = %+v, want {cmdSem:0 userCmdSem:1}", seen)
	}
	if len(h.userCmdSem) != 0 {
		t.Fatalf("userCmdSem holds %d slots after the call, want it released", len(h.userCmdSem))
	}
}

// TestRunGhCommand_UsesUserPoolNotCmdSem proves runGhCommand draws from
// userCmdSem, not cmdSem — untested since gh joined the long-command pool
// (gt-d5xr) — and that the user pool really bounds it.
func TestRunGhCommand_UsesUserPoolNotCmdSem(t *testing.T) {
	h := &APIHandler{
		cmdSem:     make(chan struct{}, 1),
		userCmdSem: make(chan struct{}, 1),
		clock:      clockwork.NewFakeClock(),
	}
	var seen poolOccupancy
	h.runProc = func(ctx context.Context, cmd *exec.Cmd) error {
		if base := filepath.Base(cmd.Path); base != "gh" {
			t.Errorf("runGhCommand ran %q, want gh", cmd.Path)
		}
		return occupancyProbe(h, &seen, "ok")(ctx, cmd)
	}

	out, err := h.runGhCommand(context.Background(), time.Minute, []string{"pr", "list"})
	if err != nil {
		t.Fatalf("runGhCommand: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want %q", out, "ok")
	}
	if seen != (poolOccupancy{cmdSem: 0, userCmdSem: 1}) {
		t.Fatalf("pools held while gh ran = %+v, want {cmdSem:0 userCmdSem:1}", seen)
	}

	// With userCmdSem full, it must fail waiting for a slot once its wait
	// budget passes on the clock, rather than run unbounded.
	clock := clockwork.NewFakeClock()
	h.clock = clock
	h.slotWaitBudget = 20 * time.Millisecond
	h.userCmdSem <- struct{}{}
	res := make(chan error, 1)
	go func() {
		_, err := h.runGhCommand(context.Background(), time.Minute, []string{"pr", "list"})
		res <- err
	}()
	blockUntilWaiters(t, clock, 1)
	clock.Advance(h.slotWaitBudget)
	if err := <-res; err == nil || !strings.Contains(err.Error(), "command slot unavailable") {
		t.Fatalf("runGhCommand with userCmdSem full = %v, want a slot-wait error", err)
	}
}

// blockUntilWaiters waits until clock has n pending timers — here, a queued
// call's slot-wait deadline — so the test advances time only once the call
// is actually waiting.
func blockUntilWaiters(t *testing.T, clock *clockwork.FakeClock, n int) {
	t.Helper()
	if err := clock.BlockUntilContext(t.Context(), n); err != nil {
		t.Fatalf("waiting for %d clock waiter(s): %v", n, err)
	}
}

// TestRunBdCmd_BoundsConcurrentSubprocesses is the regression test for
// gt-d5xr: fetchAndRender fires 17 fetcher goroutines at once, each
// eventually calling runBdCmd, and nothing bounded how many of those ran as
// real OS processes at the same time. bd/Dolt contention is fsync-bound, so
// a large herd of concurrent bd children makes each one slower instead of
// finishing sooner (gt-05vk measured this directly).
//
// Both subtests fire the same 17 calls through runBdCmd with children that
// block until the test releases them; only cmdSem differs. The unbounded
// case is the pre-fix control: every call reaches its child at once, which
// proves the harness can see a herd, so the bounded case cannot pass
// vacuously.
func TestRunBdCmd_BoundsConcurrentSubprocesses(t *testing.T) {
	const numCalls = 17

	run := func(t *testing.T, sem chan struct{}, holdUntil int) (peak, calls int) {
		t.Helper()
		procs, release := newGatedProcs(numCalls)
		f := &LiveConvoyFetcher{
			cmdTimeout: 10 * time.Second,
			cmdSem:     sem,
			clock:      clockwork.NewFakeClock(),
			runProc:    procs.run,
		}

		var wg sync.WaitGroup
		wg.Add(numCalls)
		for i := 0; i < numCalls; i++ {
			go func() {
				defer wg.Done()
				if _, err := f.runBdCmd(t.TempDir(), "show", "gt-d5xr"); err != nil {
					t.Errorf("runBdCmd: %v", err)
				}
			}()
		}
		awaitEntered(t, procs, holdUntil)
		release()
		wg.Wait()
		return procs.stats()
	}

	t.Run("unbounded (pre-fix control)", func(t *testing.T) {
		peak, calls := run(t, nil, numCalls)
		if calls != numCalls || peak != numCalls {
			t.Fatalf("unbounded: %d calls, peak %d; want all %d in flight at once", calls, peak, numCalls)
		}
	})

	t.Run("bounded", func(t *testing.T) {
		const bound = 4
		peak, calls := run(t, make(chan struct{}, bound), bound)
		if calls != numCalls {
			t.Fatalf("bounded: %d calls ran, want %d", calls, numCalls)
		}
		if peak != bound {
			t.Fatalf("bounded: peak concurrent bd children = %d, want exactly %d", peak, bound)
		}
	})
}

// TestRunGtCmd_BoundsConcurrentSubprocesses mirrors the bd case for the
// fetcher's long-running gt children (the background polecat-inventory
// refresh), which draw from userCmdSem — the pool the partition keeps
// separate from the bd-read cmdSem (gt-d5xr).
func TestRunGtCmd_BoundsConcurrentSubprocesses(t *testing.T) {
	const numCalls = 8
	const bound = 3

	procs, release := newGatedProcs(numCalls)
	f := &LiveConvoyFetcher{
		userCmdSem: make(chan struct{}, bound),
		clock:      clockwork.NewFakeClock(),
		runProc:    procs.run,
	}

	var wg sync.WaitGroup
	wg.Add(numCalls)
	for i := 0; i < numCalls; i++ {
		go func() {
			defer wg.Done()
			if _, err := f.runGtCmd(10*time.Second, "polecat", "list"); err != nil {
				t.Errorf("runGtCmd: %v", err)
			}
		}()
	}
	awaitEntered(t, procs, bound)
	release()
	wg.Wait()

	peak, calls := procs.stats()
	if calls != numCalls {
		t.Fatalf("%d gt calls ran, want %d", calls, numCalls)
	}
	if peak != bound {
		t.Fatalf("peak concurrent gt children = %d, want exactly %d", peak, bound)
	}
}

// TestTownMergeQueueSnapshot_RoundsThroughBDPool proves the merge-queue
// snapshot's bd list seam draws from (and releases back to) the fetcher's
// bd-read pool. The seam shells out through internal/beads, which resolves
// bd from PATH, so the probe is behavioral: with the pool full, the seam
// must time out waiting for a slot (the slot-wait error path), and with the
// pool drained it must run its list again.
func TestTownMergeQueueSnapshot_RoundsThroughBDPool(t *testing.T) {
	town := t.TempDir()
	rigDir := filepath.Join(town, "test-rig")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatalf("make rig dir: %v", err)
	}
	mayorDir := filepath.Join(town, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatalf("make mayor dir: %v", err)
	}
	rigsJSON := `{"version":1,"rigs":{"test-rig":{"git_url":"https://example.com/test-rig.git","added_at":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(rigsJSON), 0o644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}

	f := &LiveConvoyFetcher{
		townRoot:   town,
		cmdTimeout: 10 * time.Second,
		cmdSem:     make(chan struct{}, 1),
		clock:      clockwork.NewFakeClock(),
	}

	// A failing lister: no bd subprocess to observe, but it proves whether
	// the seam got its slot at all.
	var listCalls int64
	f.listMRs = func(string, beads.ListOptions) ([]*beads.Issue, error) {
		atomic.AddInt64(&listCalls, 1)
		return nil, errors.New("probe list failure")
	}

	// Pool drained: the seam acquires, runs the list, releases.
	if _, err := f.townMergeQueueSnapshot(); err == nil {
		t.Fatal("snapshot succeeded, want the probe's failure — the list seam did not run")
	}
	if got := atomic.LoadInt64(&listCalls); got != 1 {
		t.Fatalf("pool drained: list ran %d times, want 1", got)
	}

	// Pool full: the seam must fail without ever running the list —
	// evidence it is acquiring against the shared pool, not escaping it.
	// The slot never frees, so the wait ends only when the test moves the
	// clock past its budget (gt-d5xr).
	clock := clockwork.NewFakeClock()
	f.clock = clock
	f.cmdSem <- struct{}{}
	res := make(chan error, 1)
	go func() {
		_, err := f.townMergeQueueSnapshot()
		res <- err
	}()
	blockUntilWaiters(t, clock, 1)
	clock.Advance(f.cmdTimeout)
	if err := <-res; err == nil {
		t.Fatal("snapshot succeeded with the pool full, want the slot-wait failure")
	}
	if got := atomic.LoadInt64(&listCalls); got != 1 {
		t.Fatalf("pool full: list ran %d times total, want still 1 — the seam escaped the pool", got)
	}
}

// TestWaitBudget_DefaultsToExecTimeout is the unit-level proof that both
// LiveConvoyFetcher and APIHandler tie their default slot-wait budget to
// the call's own exec timeout rather than a fixed constant (gt-d5xr), and
// that both still accept an override for tests.
func TestWaitBudget_DefaultsToExecTimeout(t *testing.T) {
	f := &LiveConvoyFetcher{}
	if got := f.waitBudget(20 * time.Second); got != 20*time.Second {
		t.Fatalf("fetcher waitBudget(20s) = %v, want 20s (tied to the call's own timeout)", got)
	}
	f.slotWaitBudget = 2 * time.Second
	if got := f.waitBudget(20 * time.Second); got != 2*time.Second {
		t.Fatalf("fetcher waitBudget with override = %v, want 2s", got)
	}

	h := &APIHandler{}
	if got := h.waitBudget(20 * time.Second); got != 20*time.Second {
		t.Fatalf("APIHandler waitBudget(20s) = %v, want 20s (tied to the call's own timeout)", got)
	}
	h.slotWaitBudget = 2 * time.Second
	if got := h.waitBudget(20 * time.Second); got != 2*time.Second {
		t.Fatalf("APIHandler waitBudget with override = %v, want 2s", got)
	}
}

// TestRunBdCmd_SlotWaitScalesWithCmdTimeout is the saturated-pool proof: a
// call queued behind a full pool waits for a slot as long as its own
// cmdTimeout, not a fixed constant independent of it (gt-d5xr), and an
// explicit slotWaitBudget still bounds the wait. Time moves only when the
// test advances the fake clock, so a loaded host cannot spend the budget.
func TestRunBdCmd_SlotWaitScalesWithCmdTimeout(t *testing.T) {
	const cmdTimeout = 2 * time.Second

	queueBehindFullPool := func(t *testing.T, slotWaitBudget time.Duration) (*clockwork.FakeClock, chan struct{}, <-chan error) {
		t.Helper()
		clock := clockwork.NewFakeClock()
		sem := make(chan struct{}, 1)
		sem <- struct{}{} // the one slot is held by the test
		f := &LiveConvoyFetcher{
			cmdTimeout:     cmdTimeout,
			slotWaitBudget: slotWaitBudget,
			cmdSem:         sem,
			clock:          clock,
			runProc:        (&fakeProcs{}).run,
		}
		res := make(chan error, 1)
		go func() {
			_, err := f.runBdCmd(t.TempDir(), "show", "gt-d5xr")
			res <- err
		}()
		blockUntilWaiters(t, clock, 1) // the call is queued on its slot wait
		return clock, sem, res
	}

	t.Run("a queued call waits its full cmdTimeout for a slot", func(t *testing.T) {
		clock, sem, res := queueBehindFullPool(t, 0)
		clock.Advance(cmdTimeout - time.Nanosecond)
		select {
		case err := <-res:
			t.Fatalf("queued call returned %v before its cmdTimeout of slot wait elapsed", err)
		default:
		}
		<-sem // the slot frees up just inside the budget
		if err := <-res; err != nil {
			t.Fatalf("queued call failed after the slot freed within cmdTimeout: %v", err)
		}
	})

	t.Run("the wait ends at cmdTimeout", func(t *testing.T) {
		clock, _, res := queueBehindFullPool(t, 0)
		clock.Advance(cmdTimeout)
		if err := <-res; err == nil || !strings.Contains(err.Error(), "waiting for subprocess slot") {
			t.Fatalf("queued call after cmdTimeout of waiting = %v, want a slot-wait error", err)
		}
	})

	t.Run("an explicit override still bounds the wait", func(t *testing.T) {
		const budget = 10 * time.Millisecond
		clock, _, res := queueBehindFullPool(t, budget)
		clock.Advance(budget)
		if err := <-res; err == nil || !strings.Contains(err.Error(), "waiting for subprocess slot") {
			t.Fatalf("queued call after its %v override = %v, want a slot-wait error", budget, err)
		}
	})
}

// TestRunBdCmd_DeadlineEndsARunningChild proves the exec deadline: a child
// that would run forever ends when cmdTimeout passes on the clock, and the
// error names the timeout (gt-huzu reads it as "detail unavailable").
func TestRunBdCmd_DeadlineEndsARunningChild(t *testing.T) {
	clock := clockwork.NewFakeClock()
	started := make(chan struct{})
	f := &LiveConvoyFetcher{
		cmdTimeout: 300 * time.Millisecond,
		clock:      clock,
		runProc: func(ctx context.Context, _ *exec.Cmd) error {
			close(started)
			<-ctx.Done() // runs until the deadline kills it
			return errors.New("signal: killed")
		},
	}
	res := make(chan error, 1)
	go func() {
		_, err := f.runBdCmd(t.TempDir(), "dep", "list", "hq-cv-slow")
		res <- err
	}()
	<-started
	clock.Advance(f.cmdTimeout)
	if err := <-res; err == nil || err.Error() != "bd timed out after 300ms" {
		t.Fatalf("runBdCmd past its deadline = %v, want %q", err, "bd timed out after 300ms")
	}
}

// TestRunBdCmd_DefaultRunnerKillsChildAtDeadline covers the production
// runner, which the tests above replace: exec.CommandContext must kill a real
// child at the deadline. The child spins until killed and never exits on its
// own, so the deadline is the only way the call can end, however loaded the
// host is. It spins only while its parent lives, so a test binary killed
// mid-run cannot orphan it.
func TestRunBdCmd_DefaultRunnerKillsChildAtDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}
	bdPath := filepath.Join(t.TempDir(), "bd")
	script := "#!/bin/sh\nwhile kill -0 \"$PPID\" 2>/dev/null; do :; done\n"
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	f := &LiveConvoyFetcher{cmdTimeout: 200 * time.Millisecond, bdBin: bdPath}
	if _, err := f.runBdCmd(t.TempDir(), "show", "gt-d5xr"); err == nil || !strings.Contains(err.Error(), "bd timed out after") {
		t.Fatalf("runBdCmd on a child that never exits = %v, want a timeout error", err)
	}
}

// TestAcquireCmdSlot_FreeSlotWinsOverExpiredWait pins acquireCmdSlot's
// tie-break: when a slot is free, the call gets it even if its wait budget has
// already run out. A bare select over the slot send and ctx.Done() picks at
// random when both are ready, so a caller descheduled past its budget failed
// with "waiting for subprocess slot" on an idle pool about half the time.
func TestAcquireCmdSlot_FreeSlotWinsOverExpiredWait(t *testing.T) {
	sem := make(chan struct{}, 1)
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 64; i++ {
		if err := acquireCmdSlot(expired, sem); err != nil {
			t.Fatalf("attempt %d: acquireCmdSlot on a free pool with an expired wait = %v, want the free slot", i, err)
		}
		releaseCmdSlot(sem)
	}

	// A full pool still honours the expired wait.
	sem <- struct{}{}
	if err := acquireCmdSlot(expired, sem); err == nil {
		t.Fatal("acquireCmdSlot on a full pool with an expired wait = nil, want the wait error")
	}
}
