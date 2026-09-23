package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewDashboardMux_SharesSubprocessSemaphore is the regression test for
// the design mistake behind an earlier, insufficient version of the gt-d5xr
// fix: bounding fetchAndRender's fan-out alone left APIHandler drawing bd/gt
// subprocess slots from its own separate pool (cmdSem, sized independently
// in NewAPIHandler), so the two handlers' fetches could still overlap
// without limit. NewDashboardMux must hand APIHandler the same channel the
// fetcher uses, not a fresh one.
func TestNewDashboardMux_SharesSubprocessSemaphore(t *testing.T) {
	fetcher := &LiveConvoyFetcher{cmdSem: make(chan struct{}, 4)}

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

	if apiHandler.cmdSem == nil {
		t.Fatal("apiHandler.cmdSem is nil — not sharing the fetcher's semaphore")
	}
	// Channels compare equal only when they refer to the same underlying
	// channel, so this proves it is the identical pool, not a same-sized copy.
	if apiHandler.cmdSem != fetcher.cmdSem { //nolint:staticcheck // intentional channel identity check
		t.Fatal("apiHandler.cmdSem is a different channel than the fetcher's — subprocess fan-out can still overlap unbounded")
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
// eventually calling runBdCmd or runGtCmd, and nothing bounded how many of
// those ran as real OS processes at the same time. bd/Dolt contention is
// fsync-bound, so a large herd of concurrent bd children makes each one
// slower instead of finishing sooner (gt-05vk measured this directly).
//
// Both subtests fire the same 17 concurrent calls — matching
// fetchAndRender's wg.Add(17) — through the same runBdCmd path; only cmdSem
// differs. The unbounded case is the pre-fix control: it proves the herd is
// real (peak reaches all 17) rather than an artifact of the harness. A test
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
		peak, calls := run(t, nil)
		if calls != numCalls {
			t.Fatalf("vacuous-pass guard: got %d calls, want %d — probe did not run", calls, numCalls)
		}
		if peak != numCalls {
			t.Fatalf("peak concurrent bd children = %d, want %d (the unbounded case should show the full herd)", peak, numCalls)
		}
		t.Logf("unbounded: peak concurrent bd children = %d over %d calls", peak, calls)
	})

	t.Run("bounded", func(t *testing.T) {
		const bound = 4
		peak, calls := run(t, make(chan struct{}, bound))
		if calls != numCalls {
			t.Fatalf("vacuous-pass guard: got %d calls, want %d — probe did not run", calls, numCalls)
		}
		if peak > bound {
			t.Fatalf("peak concurrent bd children = %d, want at most %d", peak, bound)
		}
		t.Logf("bounded: peak concurrent bd children = %d over %d calls (bound %d)", peak, calls, bound)
	})
}

// TestRunGtCmd_BoundsConcurrentSubprocesses mirrors the bd case for gt
// subprocesses (e.g. the background polecat-inventory refresh), which use a
// separate runner (runGtCmd) but the same cmdSem.
func TestRunGtCmd_BoundsConcurrentSubprocesses(t *testing.T) {
	const numCalls = 8
	const sleep = 150 * time.Millisecond
	const bound = 3

	binDir := t.TempDir()
	runsDir := t.TempDir()
	gtPath := filepath.Join(binDir, "gt")
	writeConcurrencyProbeScript(t, gtPath, runsDir, sleep)

	f := &LiveConvoyFetcher{
		gtBin:  gtPath,
		cmdSem: make(chan struct{}, bound),
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
	if peak > bound {
		t.Fatalf("peak concurrent gt children = %d, want at most %d", peak, bound)
	}
	t.Logf("bounded: peak concurrent gt children = %d over %d calls (bound %d)", peak, calls, bound)
}
