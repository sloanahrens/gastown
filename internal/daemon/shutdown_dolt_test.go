package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// shutdownTestDaemon is a daemon with the context pair shutdown() cancels and
// a town root under t.TempDir, so SaveState has somewhere to write.
func shutdownTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
		ctx:    ctx,
		cancel: cancel,
	}
}

// shuttingDownTestManager is a manager whose server is down and whose start is
// counted, so a test can tell "brought Dolt up" from "left it alone".
func shuttingDownTestManager(t *testing.T, daemon *Daemon, starts *atomic.Int32) *DoltServerManager {
	t.Helper()
	return &DoltServerManager{
		config:   &DoltServerConfig{Enabled: true},
		townRoot: daemon.config.TownRoot,
		logger:   func(string, ...interface{}) {},
		runningFn: func() (int, bool) {
			return 0, false
		},
		startFn:    func() error { starts.Add(1); return nil },
		stopFn:     func() {},
		sleepFn:    func(time.Duration) {},
		nowFn:      time.Now,
		escalateFn: func(int) {},
	}
}

// A SIGTERM'd daemon must cancel its context before it stops Dolt: every
// background worker stops on d.ctx.Done(), and the ones that bring Dolt up
// themselves (compactor dog, scheduled slings) would otherwise keep starting
// passes against a server that is going down (gt-govth).
func TestShutdownCancelsContextBeforeStoppingDolt(t *testing.T) {
	t.Parallel()
	d := shutdownTestDaemon(t)

	workerReturned := make(chan struct{})
	go func() {
		defer close(workerReturned)
		<-d.ctx.Done()
	}()

	stopCalled := false
	ctxCanceledAtStop := false
	d.doltServer = &DoltServerManager{
		config:   &DoltServerConfig{Enabled: true},
		townRoot: d.config.TownRoot,
		logger:   func(string, ...interface{}) {},
		stopFn: func() {
			stopCalled = true
			ctxCanceledAtStop = d.ctx.Err() != nil
		},
	}

	if err := d.shutdown(&State{Running: true}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if !stopCalled {
		t.Fatal("shutdown never stopped the Dolt server it manages")
	}
	if !ctxCanceledAtStop {
		t.Error("Dolt was stopped while the daemon context was still live: workers can still act during the stop")
	}
	waitForSignal(t, workerReturned, "a worker blocked on d.ctx.Done() did not return when shutdown started")
}

// Shutdown closes off the sandbox seat ends in flight before it stops Dolt:
// their remaining reads go through Dolt, and an end left running would keep
// reading it on the way down.
func TestShutdownStopsTheSeatEndsBeforeStoppingDolt(t *testing.T) {
	t.Parallel()
	d := shutdownTestDaemon(t)

	stopCalled := false
	closedBeforeStop := false
	d.doltServer = &DoltServerManager{
		config:   &DoltServerConfig{Enabled: true},
		townRoot: d.config.TownRoot,
		logger:   func(string, ...interface{}) {},
		stopFn: func() {
			stopCalled = true
			closedBeforeStop = d.sandboxSeatEndStopped
		},
	}

	if err := d.shutdown(&State{Running: true}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if !stopCalled {
		t.Fatal("shutdown never stopped the Dolt server it manages")
	}
	if !closedBeforeStop {
		t.Error("Dolt was stopped while the seat ends could still start")
	}
}

// The bring-up a patrol cycle calls must not start Dolt once shutdown has
// begun; the same daemon before shutdown still starts it, so a passing test is
// not a manager that never starts anything.
func TestEnsureDoltServerUpStartsNoDoltAfterShutdown(t *testing.T) {
	t.Parallel()

	t.Run("not shutting down", func(t *testing.T) {
		t.Parallel()
		var starts atomic.Int32
		d := shutdownTestDaemon(t)
		d.doltServer = shuttingDownTestManager(t, d, &starts)

		if err := d.ensureDoltServerUp(); err != nil {
			t.Fatalf("ensureDoltServerUp: %v", err)
		}
		if got := starts.Load(); got != 1 {
			t.Fatalf("Dolt starts = %d, want 1 (this is the control: nothing here is blocked)", got)
		}
	})

	t.Run("shutdown begun", func(t *testing.T) {
		t.Parallel()
		var starts atomic.Int32
		d := shutdownTestDaemon(t)
		d.doltServer = shuttingDownTestManager(t, d, &starts)

		d.cancel() // what shutdown() does first

		// The skip is reported, not returned as success: callers that need
		// the server cannot tell "up" from "left alone" otherwise (gt-84o0a).
		if err := d.ensureDoltServerUp(); !errors.Is(err, errDoltBringUpSkipped) {
			t.Fatalf("ensureDoltServerUp = %v, want errDoltBringUpSkipped", err)
		}
		if got := starts.Load(); got != 0 {
			t.Fatalf("Dolt starts = %d, want 0: the daemon is going down and would leave a detached server behind", got)
		}
	})
}

// The compactor dog does not run its SQL cycle, or record a run, when the Dolt
// bring-up was skipped for shutdown; the same daemon before shutdown runs it
// (gt-84o0a).
func TestCompactorDogSkipsItsCycleWhenDoltBringUpIsSkipped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		shuttingDown bool
		wantCycles   int32
	}{
		{"daemon running", false, 1},
		{"daemon shutting down", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var starts, cycles atomic.Int32
			d := compactorDogTestDaemon(t.TempDir(), nil)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			d.ctx, d.cancel = ctx, cancel
			d.doltServer = shuttingDownTestManager(t, d, &starts)
			d.compactorDogCycleFn = func() bool { cycles.Add(1); return true }
			if tc.shuttingDown {
				cancel()
			}
			d.triggerCompactorDog()
			awaitCompactorDogIdle(t, d)
			if got := cycles.Load(); got != tc.wantCycles {
				t.Errorf("cycles = %d, want %d", got, tc.wantCycles)
			}
		})
	}
}

// holdShutdownLock takes the flock `gt down` holds while it tears the town down.
func holdShutdownLock(t *testing.T, townRoot string) {
	t.Helper()
	lockPath := filepath.Join(townRoot, "daemon", "shutdown.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(lockPath)
	if locked, err := lock.TryLock(); err != nil || !locked {
		t.Fatalf("TryLock shutdown.lock = %v, %v", locked, err)
	}
	t.Cleanup(func() { _ = lock.Unlock() })
}

// A daemon started while `gt down` holds shutdown.lock has a live context, so
// the context check alone lets its startup catch-ups bring Dolt up in a town
// being torn down. The bring-up honors the lock, and the compactor catch-up that
// calls it runs no cycle and starts no server (gt-t4j0m).
func TestStartupCatchUpHonorsShutdownLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		holdLock   bool
		wantStarts int32
		wantCycles int32
	}{
		{"no shutdown", false, 1, 1},
		{"shutdown.lock held", true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var starts, cycles atomic.Int32
			d := compactorDogTestDaemon(t.TempDir(), nil)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			d.ctx, d.cancel = ctx, cancel
			d.doltServer = shuttingDownTestManager(t, d, &starts)
			d.compactorDogCycleFn = func() bool { cycles.Add(1); return true }
			if tc.holdLock {
				holdShutdownLock(t, d.config.TownRoot)
				if err := d.ensureDoltServerUp(); !errors.Is(err, errDoltBringUpSkipped) {
					t.Fatalf("ensureDoltServerUp = %v, want errDoltBringUpSkipped", err)
				}
			}

			d.triggerCompactorDog()
			awaitCompactorDogIdle(t, d)

			if got := cycles.Load(); got != tc.wantCycles {
				t.Errorf("compactor cycles = %d, want %d", got, tc.wantCycles)
			}
			if got := starts.Load(); got != tc.wantStarts {
				t.Errorf("Dolt starts = %d, want %d", got, tc.wantStarts)
			}
		})
	}
}

// A manager that has been asked to stop stays stopped: this is the race the
// daemon-level cancel alone cannot close, because a worker that reached
// EnsureRunning before the cancel blocks on mu until Stop releases it.
func TestEnsureRunning_StartRequestedAfterStopIsRefused(t *testing.T) {
	t.Parallel()
	var starts atomic.Int32
	m := newTestManager(t)
	m.startFn = func() error { starts.Add(1); return nil }

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got := starts.Load(); got != 0 {
		t.Fatalf("Dolt starts after Stop = %d, want 0", got)
	}
}

// The backoff sleep releases mu, so it is the window where a Stop can land
// between EnsureRunning's entry and its start. Stop is called from inside the
// sleep here: deterministic, and it lands exactly there.
func TestEnsureRunning_StopDuringBackoffStartIsRefused(t *testing.T) {
	t.Parallel()
	var starts, stops atomic.Int32
	m := newTestManager(t)
	m.startFn = func() error { starts.Add(1); return nil }
	m.stopFn = func() { stops.Add(1) }
	m.sleepFn = func(time.Duration) { _ = m.Stop() }

	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got := stops.Load(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1 (the test did not land a Stop in the backoff)", got)
	}
	if got := starts.Load(); got != 0 {
		t.Fatalf("Dolt starts = %d, want 0: the restart outlived the Stop that asked for it", got)
	}
}
