//go:build !windows

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/jonboulle/clockwork"
)

// gt-fsgb1: daemon.lock is probed shared and taken exclusively. These tests pin
// both halves of that split: a probe is not a daemon to another probe, and a
// start waits a probe out instead of exiting as if a rival were up.

// probeStepTimeout bounds a step that waits for the acquisition under test to
// park on the fake clock.
const probeStepTimeout = 10 * time.Second

// startProbe takes the lock the way IsRunning and FindOrphanedDaemons do, and
// refuses a town a daemon turned out to hold.
func startProbe(t *testing.T, townRoot string) *flock.Flock {
	t.Helper()
	held, probeLock, err := probeDaemonLock(daemonLockPath(townRoot))
	if err != nil {
		t.Fatalf("probeDaemonLock: %v", err)
	}
	if held {
		t.Fatal("probeDaemonLock reported a daemon in a town no daemon holds")
	}
	if probeLock == nil {
		t.Fatal("probeDaemonLock returned no shared lock for a town with a lock file")
	}
	return probeLock
}

// waitForParkedStart waits for the acquisition under test to be asleep on clk.
// Only it uses the clock, so a waiter is that goroutine parked between
// attempts.
func waitForParkedStart(t *testing.T, clk *clockwork.FakeClock) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), probeStepTimeout)
	defer cancel()
	return clk.BlockUntilContext(ctx, 1) == nil
}

// advanceFakeClock advances clk by d once the acquisition under test is parked
// on it, and reports whether anything was parked.
func advanceFakeClock(t *testing.T, clk *clockwork.FakeClock, d time.Duration) bool {
	t.Helper()
	if !waitForParkedStart(t, clk) {
		return false
	}
	clk.Advance(d)
	return true
}

// Two probes of daemon.lock must not see each other as a daemon: an exclusive
// probe would, and the town would report a daemon that is not there (gt-fsgb1).
func TestDaemonLockProbesDoNotBlockEachOther(t *testing.T) {
	t.Parallel()
	townRoot := lockTown(t)

	first := startProbe(t, townRoot)
	defer func() { _ = first.Unlock() }()

	held, second, err := probeDaemonLock(daemonLockPath(townRoot))
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if held {
		t.Error("the second probe saw the first probe as a daemon")
	}
	if second == nil {
		t.Error("the second probe took no shared lock")
	} else {
		_ = second.Unlock()
	}
}

// Everything a probe does to a town's pid file happens under the lock it read
// that file under, so a daemon cannot take the lock and write a pid file the
// probe then deletes (gt-fsgb1).
func TestDaemonCannotTakeTheLockUnderAProbe(t *testing.T) {
	t.Parallel()
	townRoot := lockTown(t)

	probeLock := startProbe(t, townRoot)

	daemon := flock.New(daemonLockPath(townRoot))
	switch locked, err := daemon.TryLock(); {
	case err != nil:
		t.Fatalf("daemon lock: %v", err)
	case locked:
		_ = daemon.Unlock()
		t.Fatal("a daemon took the lock while a probe held it")
	}

	_ = probeLock.Unlock()

	locked, err := daemon.TryLock()
	if err != nil {
		t.Fatalf("daemon lock after the probe released it: %v", err)
	}
	if !locked {
		t.Fatal("the lock did not free when the probe released it")
	}
	_ = daemon.Unlock()
}

// A probe in flight is not a rival daemon: a start that loses the race to one
// waits it out (gt-fsgb1).
func TestDaemonStartWaitsOutAProbe(t *testing.T) {
	t.Parallel()
	townRoot := lockTown(t)
	clk := clockwork.NewFakeClockAt(stopWaitEpoch)

	probeLock := startProbe(t, townRoot)

	type result struct {
		lock *flock.Flock
		err  error
	}
	started := make(chan result, 1)
	go func() {
		l, err := acquireDaemonLock(clk, daemonLockPath(townRoot), daemonLockWaitBudget, daemonLockPollInterval)
		started <- result{lock: l, err: err}
	}()

	// One retry, spent against the probe: the start is asleep again, waiting.
	if !advanceFakeClock(t, clk, daemonLockPollInterval) {
		t.Fatal("the start did not retry against the probe")
	}
	if !waitForParkedStart(t, clk) {
		t.Fatal("the start stopped retrying while the probe still held the lock")
	}

	// The probe settles the pid file and releases: the next retry takes the lock.
	_ = probeLock.Unlock()
	if !advanceFakeClock(t, clk, daemonLockPollInterval) {
		t.Fatal("the start stopped retrying after the probe released the lock")
	}

	ctx, cancel := context.WithTimeout(t.Context(), probeStepTimeout)
	defer cancel()
	select {
	case got := <-started:
		if got.err != nil {
			t.Fatalf("the start gave up on a probe: %v", got.err)
		}
		_ = got.lock.Unlock()
	case <-ctx.Done():
		t.Fatal("the start neither took the lock nor gave up")
	}
}

// A daemon that holds the lock for its whole life is still refused: the wait
// accommodates probes, not rivals (gt-fsgb1).
func TestDaemonStartRefusesALiveDaemon(t *testing.T) {
	t.Parallel()
	townRoot := lockTown(t)
	clk := clockwork.NewFakeClockAt(stopWaitEpoch)

	// A daemon holds the lock for as long as it runs.
	rival := flock.New(daemonLockPath(townRoot))
	if locked, err := rival.TryLock(); err != nil || !locked {
		t.Fatalf("taking the lock as the running daemon: locked=%v err=%v", locked, err)
	}
	defer func() { _ = rival.Unlock() }()

	budget := 3 * daemonLockPollInterval
	failed := make(chan error, 1)
	go func() {
		lock, err := acquireDaemonLock(clk, daemonLockPath(townRoot), budget, daemonLockPollInterval)
		if lock != nil {
			_ = lock.Unlock()
		}
		failed <- err
	}()

	// Each retry burns one poll; the attempt past the budget gives up.
	for range 3 {
		if !advanceFakeClock(t, clk, daemonLockPollInterval) {
			t.Fatal("the start stopped retrying before its budget was out")
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), probeStepTimeout)
	defer cancel()
	select {
	case err := <-failed:
		if err == nil || !strings.Contains(err.Error(), "already running") {
			t.Fatalf("start against a live daemon failed with %v, want an already-running error", err)
		}
	case <-ctx.Done():
		t.Fatal("the start never gave up on a live daemon")
	}
}
