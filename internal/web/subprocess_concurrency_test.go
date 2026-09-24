package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestUserCommandPoolDoesNotStarveFetcherBD is the behavioral proof of the
// partition, driving both handlers it claims are independent: with the
// APIHandler's cmdSem completely full — as a burst of options/mail/ready
// fetches would leave it — a user-driven command must still run, because it
// draws from userCmdSem only (see runUserGtCommand); meanwhile the
// fetcher's own bd-read pool, a separate object entirely, keeps servicing
// its full herd of short reads (gt-d5xr).
func TestUserCommandPoolDoesNotStarveFetcherBD(t *testing.T) {
	const numCalls = 17
	const sleep = 50 * time.Millisecond

	binDir := t.TempDir()
	runsDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	writeConcurrencyProbeScript(t, bdPath, runsDir, sleep)

	gtPath := filepath.Join(binDir, "gt")
	if err := os.WriteFile(gtPath, []byte("#!/bin/sh\nprintf 'ok'\n"), 0o755); err != nil {
		t.Fatalf("write fake gt binary: %v", err)
	}

	h := &APIHandler{
		gtPath:     gtPath,
		cmdSem:     make(chan struct{}, maxConcurrentCommands),
		userCmdSem: make(chan struct{}, userCommandConcurrency),
	}
	// Fill the short-read pool completely, as a burst of API reads would.
	for i := 0; i < maxConcurrentCommands; i++ {
		h.cmdSem <- struct{}{}
	}

	f := &LiveConvoyFetcher{
		cmdTimeout: 10 * time.Second,
		bdBin:      bdPath,
		cmdSem:     make(chan struct{}, subprocessConcurrency),
	}

	stop := make(chan struct{})
	peakPtr, watcherDone := watchPeakConcurrency(runsDir, stop)

	var wg sync.WaitGroup
	var callCount int64
	wg.Add(numCalls)
	for i := 0; i < numCalls; i++ {
		go func() {
			defer wg.Done()
			if _, err := f.runBdCmd(t.TempDir(), "show", "gt-d5xr"); err != nil {
				t.Errorf("runBdCmd with cmdSem full: %v", err)
				return
			}
			atomic.AddInt64(&callCount, 1)
		}()
	}

	// A user-driven command must succeed concurrently with the bd herd
	// above, even though every cmdSem slot is taken — it never touches
	// cmdSem (gt-d5xr).
	out, err := h.runUserGtCommand(context.Background(), time.Second, []string{"rig", "add"})

	wg.Wait()
	close(stop)
	<-watcherDone

	if err != nil {
		t.Fatalf("runUserGtCommand with cmdSem full: %v", err)
	}
	if out != "ok" {
		t.Fatalf("runUserGtCommand output = %q, want %q", out, "ok")
	}

	peak, calls := atomic.LoadInt64(peakPtr), int(atomic.LoadInt64(&callCount))
	if calls != numCalls {
		t.Fatalf("got %d bd calls with cmdSem full, want %d — the bd reads starved", calls, numCalls)
	}
	if peak > subprocessConcurrency {
		t.Fatalf("peak concurrent bd children = %d, want at most %d", peak, subprocessConcurrency)
	}
	t.Logf("cmdSem full: %d bd calls completed, peak %d (bound %d); runUserGtCommand succeeded", calls, peak, subprocessConcurrency)
}

// TestRunUserGtCommand_DoesNotDrawCmdSemSlot is the minimal regression for
// runUserGtCommand once wrapping runGtCommand, which also acquired cmdSem,
// so a long user run held a short-read slot for its whole lifetime (gt-d5xr).
// With cmdSem's only slot held, and never released, the call must still
// succeed.
func TestRunUserGtCommand_DoesNotDrawCmdSemSlot(t *testing.T) {
	binDir := t.TempDir()
	gtPath := filepath.Join(binDir, "gt")
	if err := os.WriteFile(gtPath, []byte("#!/bin/sh\nprintf 'ok'\n"), 0o755); err != nil {
		t.Fatalf("write fake gt binary: %v", err)
	}

	h := &APIHandler{
		gtPath:     gtPath,
		cmdSem:     make(chan struct{}, 1),
		userCmdSem: make(chan struct{}, 1),
	}
	h.cmdSem <- struct{}{} // held for the whole test; never released

	out, err := h.runUserGtCommand(context.Background(), time.Second, []string{"rig", "add"})
	if err != nil {
		t.Fatalf("runUserGtCommand with cmdSem's only slot held: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want %q", out, "ok")
	}
}

// TestRunGhCommand_UsesUserPoolNotCmdSem proves runGhCommand draws from
// userCmdSem, not cmdSem — untested since gh joined the long-command pool
// (gt-d5xr).
func TestRunGhCommand_UsesUserPoolNotCmdSem(t *testing.T) {
	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nprintf 'ok'\n"), 0o755); err != nil {
		t.Fatalf("write fake gh binary: %v", err)
	}
	origPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })

	h := &APIHandler{
		cmdSem:         make(chan struct{}, 1),
		userCmdSem:     make(chan struct{}, 1),
		slotWaitBudget: 20 * time.Millisecond,
	}
	h.cmdSem <- struct{}{} // full short-read pool must not block gh

	out, err := h.runGhCommand(context.Background(), time.Second, []string{"pr", "list"})
	if err != nil {
		t.Fatalf("runGhCommand with cmdSem full: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want %q", out, "ok")
	}

	// But the user pool does bound it: with userCmdSem also full, it must
	// fail waiting for a slot rather than run unbounded.
	h.userCmdSem <- struct{}{}
	if _, err := h.runGhCommand(context.Background(), time.Second, []string{"pr", "list"}); err == nil {
		t.Fatal("runGhCommand succeeded with userCmdSem full, want a slot-wait error")
	}
}

// writeConcurrencyProbeScript writes a fake bd/gt binary that drops a marker
// file into runsDir for the duration of its (simulated) work, so a poller
// watching runsDir can observe how many invocations are in flight at once.
// Each invocation's marker is named by its own pid ($$), so concurrent
// invocations never collide on one file.
func writeConcurrencyProbeScript(t *testing.T, binPath, runsDir string, sleep time.Duration) {
	t.Helper()
	script := `#!/bin/sh
f="` + runsDir + `/run.$$"
touch "$f"
sleep ` + strconv.FormatFloat(sleep.Seconds(), 'f', -1, 64) + `
rm -f "$f"
printf '[]'
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
}

// watchPeakConcurrency polls runsDir until stop is closed, tracking the
// largest number of marker files (i.e. in-flight subprocesses) seen at once.
// It returns the running peak via the returned pointer so the caller can
// read it after stop fires and the watcher goroutine has exited.
func watchPeakConcurrency(runsDir string, stop <-chan struct{}) (peak *int64, done <-chan struct{}) {
	var p int64
	d := make(chan struct{})
	go func() {
		defer close(d)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			entries, err := os.ReadDir(runsDir)
			if err == nil {
				if n := int64(len(entries)); n > atomic.LoadInt64(&p) {
					atomic.StoreInt64(&p, n)
				}
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return &p, d
}

// TestRunBdCmd_BoundsConcurrentSubprocesses is the regression test for
// gt-d5xr: fetchAndRender fires 17 fetcher goroutines at once, each
// eventually calling runBdCmd, and nothing bounded how many of those ran as
// real OS processes at the same time. bd/Dolt contention is fsync-bound, so
// a large herd of concurrent bd children makes each one slower instead of
// finishing sooner (gt-05vk measured this directly).
//
// Both subtests fire the same 17 concurrent calls — matching
// fetchAndRender's producer count — through the same runBdCmd path; only cmdSem
// differs. The unbounded case is the pre-fix control: it proves the herd is
// real (many run at once) rather than an artifact of the harness. A test
// that only checked the bounded case could pass vacuously if, say, the
// fetcher stopped calling bd at all.
func TestRunBdCmd_BoundsConcurrentSubprocesses(t *testing.T) {
	const numCalls = 17
	const sleep = 150 * time.Millisecond

	run := func(t *testing.T, sem chan struct{}) (peak int64, calls int) {
		binDir := t.TempDir()
		runsDir := t.TempDir()
		bdPath := filepath.Join(binDir, "bd")
		writeConcurrencyProbeScript(t, bdPath, runsDir, sleep)

		f := &LiveConvoyFetcher{
			cmdTimeout: 10 * time.Second,
			bdBin:      bdPath,
			cmdSem:     sem,
		}

		stop := make(chan struct{})
		peakPtr, watcherDone := watchPeakConcurrency(runsDir, stop)

		var wg sync.WaitGroup
		var callCount int64
		wg.Add(numCalls)
		for i := 0; i < numCalls; i++ {
			go func() {
				defer wg.Done()
				if _, err := f.runBdCmd(t.TempDir(), "show", "gt-d5xr"); err != nil {
					t.Errorf("runBdCmd: %v", err)
					return
				}
				atomic.AddInt64(&callCount, 1)
			}()
		}
		wg.Wait()
		close(stop)
		<-watcherDone

		return atomic.LoadInt64(peakPtr), int(atomic.LoadInt64(&callCount))
	}

	t.Run("unbounded (pre-fix control)", func(t *testing.T) {
		const bound = 4
		peak, calls := run(t, nil)
		if calls != numCalls {
			t.Fatalf("vacuous-pass guard: got %d calls, want %d — probe did not run", calls, numCalls)
		}
		// Unbounded: the probe should let well more than the bound through.
		// A barrier-based script that forced an exact peak was tried here and
		// reverted: under real host contention (the load gt-d5xr is about)
		// its own polling starved the host of the capacity to spawn the
		// remaining processes, which is a worse flake than the timing this
		// lower-bound check tolerates.
		if peak <= 2*bound {
			t.Fatalf("unbounded: peak concurrent bd children = %d, want more than %d (2x bound) — the herd did not form", peak, 2*bound)
		}
		t.Logf("unbounded: peak concurrent bd children = %d over %d calls", peak, calls)
	})

	t.Run("bounded", func(t *testing.T) {
		const bound = 4
		peak, calls := run(t, make(chan struct{}, bound))
		if calls != numCalls {
			t.Fatalf("vacuous-pass guard: got %d calls, want %d — probe did not run", calls, numCalls)
		}
		if peak < 1 {
			t.Fatalf("vacuous-pass guard: peak concurrent bd children = 0 — probe never ran concurrently")
		}
		if peak > bound {
			t.Fatalf("peak concurrent bd children = %d, want at most %d", peak, bound)
		}
		t.Logf("bounded: peak concurrent bd children = %d over %d calls (bound %d)", peak, calls, bound)
	})
}

// TestRunGtCmd_BoundsConcurrentSubprocesses mirrors the bd case for the
// fetcher's long-running gt children (the background polecat-inventory
// refresh), which draw from userCmdSem — the pool the partition keeps
// separate from the bd-read cmdSem (gt-d5xr).
func TestRunGtCmd_BoundsConcurrentSubprocesses(t *testing.T) {
	const numCalls = 8
	const sleep = 150 * time.Millisecond
	const bound = 3

	binDir := t.TempDir()
	runsDir := t.TempDir()
	gtPath := filepath.Join(binDir, "gt")
	writeConcurrencyProbeScript(t, gtPath, runsDir, sleep)

	f := &LiveConvoyFetcher{
		gtBin:      gtPath,
		userCmdSem: make(chan struct{}, bound),
	}

	stop := make(chan struct{})
	peakPtr, watcherDone := watchPeakConcurrency(runsDir, stop)

	var wg sync.WaitGroup
	var callCount int64
	wg.Add(numCalls)
	for i := 0; i < numCalls; i++ {
		go func() {
			defer wg.Done()
			if _, err := f.runGtCmd(10*time.Second, "polecat", "list"); err != nil {
				t.Errorf("runGtCmd: %v", err)
				return
			}
			atomic.AddInt64(&callCount, 1)
		}()
	}
	wg.Wait()
	close(stop)
	<-watcherDone

	peak, calls := atomic.LoadInt64(peakPtr), int(atomic.LoadInt64(&callCount))
	if calls != numCalls {
		t.Fatalf("vacuous-pass guard: got %d calls, want %d — probe did not run", calls, numCalls)
	}
	if peak < 1 {
		t.Fatalf("vacuous-pass guard: peak concurrent gt children = 0 — probe never ran concurrently")
	}
	if peak > bound {
		t.Fatalf("peak concurrent gt children = %d, want at most %d", peak, bound)
	}
	t.Logf("bounded: peak concurrent gt children = %d over %d calls (bound %d)", peak, calls, bound)
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
	rigsJSON := `{"rigs":{"test-rig":{"name":"test-rig","path":"test-rig"}}}`
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(rigsJSON), 0o644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}

	f := &LiveConvoyFetcher{
		townRoot:   town,
		cmdTimeout: 10 * time.Second,
		cmdSem:     make(chan struct{}, 1),
		// The pool-full case below only needs to prove the seam waits on
		// this pool, not that it waits realistically long — a short
		// override keeps the case fast (gt-d5xr).
		slotWaitBudget: 20 * time.Millisecond,
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
	f.cmdSem <- struct{}{}
	if _, err := f.townMergeQueueSnapshot(); err == nil {
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

// TestRunBdCmd_SlotWaitScalesWithCmdTimeout is the saturated-pool proof:
// with cmdSem's single slot serializing numCalls reads, the last one queues
// for roughly (numCalls-1)*sleep. A cmdTimeout generously above that must
// let every call through — the wait is not capped by a fixed constant
// independent of the configured timeout (gt-d5xr).
func TestRunBdCmd_SlotWaitScalesWithCmdTimeout(t *testing.T) {
	const numCalls = 8
	const sleep = 50 * time.Millisecond // worst-case queue ~= 7*50ms = 350ms

	newFetcher := func(t *testing.T, cmdTimeout, slotWaitBudget time.Duration) *LiveConvoyFetcher {
		t.Helper()
		binDir := t.TempDir()
		bdPath := filepath.Join(binDir, "bd")
		script := "#!/bin/sh\nsleep " + strconv.FormatFloat(sleep.Seconds(), 'f', -1, 64) + "\nprintf '[]'\n"
		if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
			t.Fatalf("write fake binary: %v", err)
		}
		return &LiveConvoyFetcher{
			cmdTimeout:     cmdTimeout,
			slotWaitBudget: slotWaitBudget,
			bdBin:          bdPath,
			cmdSem:         make(chan struct{}, 1),
		}
	}

	run := func(f *LiveConvoyFetcher) (successes, failures int64) {
		var wg sync.WaitGroup
		wg.Add(numCalls)
		for i := 0; i < numCalls; i++ {
			go func() {
				defer wg.Done()
				if _, err := f.runBdCmd(t.TempDir(), "show", "gt-d5xr"); err != nil {
					atomic.AddInt64(&failures, 1)
					return
				}
				atomic.AddInt64(&successes, 1)
			}()
		}
		wg.Wait()
		return
	}

	t.Run("wait tied to a generous cmdTimeout: every queued call succeeds", func(t *testing.T) {
		f := newFetcher(t, 2*time.Second, 0)
		successes, failures := run(f)
		if failures != 0 {
			t.Fatalf("%d of %d queued calls failed, want 0", failures, numCalls)
		}
		if successes != numCalls {
			t.Fatalf("vacuous-pass guard: got %d successes, want %d", successes, numCalls)
		}
	})

	t.Run("an explicit override still bounds the wait", func(t *testing.T) {
		// Proves the budget is a real, enforced bound and not a no-op: it
		// just is not hardcoded at a value independent of cmdTimeout.
		f := newFetcher(t, 2*time.Second, 10*time.Millisecond)
		_, failures := run(f)
		if failures == 0 {
			t.Fatal("expected some calls to fail waiting for a slot with a 10ms budget behind a full pool")
		}
	})
}
