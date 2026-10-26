package installlock

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Acquire holds the lock until its release, the way the Go installer holds it
// from the swap to the end of the run.
func TestAcquireHoldsUntilTheRelease(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")

	release, err := Acquire(lock, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lockIsFree(t, lock) {
		t.Error("the lock was free while Acquire held it")
	}
	release()
	if !lockIsFree(t, lock) {
		t.Error("the lock was still held after the release")
	}
}

// A second caller waits the whole wait and then reports ErrBusy, on the
// injected clock so the test spends no wall time.
func TestAcquireReportsBusyWhenTheWaitRunsOut(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")
	release := holdLock(t, lock)
	defer release()

	clock := newFakeClock()
	got, err := acquire(lock, 2*time.Second, clock.Now, clock.Sleep)

	if !errors.Is(err, ErrBusy) {
		t.Fatalf("got error %v, want ErrBusy", err)
	}
	if got != nil {
		t.Error("a release was returned for a lock that was never taken")
	}
	if clock.total() != 2*time.Second {
		t.Errorf("waited %s over %v, want exactly the 2s wait", clock.total(), clock.slept)
	}
	for _, d := range clock.slept {
		if d > retryInterval {
			t.Errorf("slept %s in one go, want at most %s between tries", d, retryInterval)
		}
	}
}

// The lock is taken as soon as the holder lets go, inside the wait.
func TestAcquireTakesTheLockWhenItFreesDuringTheWait(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")
	release := holdLock(t, lock)

	clock := newFakeClock()
	clock.onSleep = func(n int) {
		if n == 2 {
			release() // the other install finishes during the second pause
		}
	}
	got, err := acquire(lock, 10*time.Second, clock.Now, clock.Sleep)

	if err != nil {
		t.Fatalf("acquire after the holder let go: %v", err)
	}
	if len(clock.slept) != 2 {
		t.Errorf("slept %v, want two pauses and then the lock", clock.slept)
	}
	if lockIsFree(t, lock) {
		t.Error("Acquire returned without holding the lock")
	}
	got()
}

// A lock file that cannot be opened is an error, never ErrBusy: the caller must
// not report another install for its own missing directory.
func TestAcquireErrorsWhenTheLockFileCannotBeOpened(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "no-such-dir", "install-gt.lock")

	release, err := Acquire(lock, time.Second)

	if err == nil {
		release()
		t.Fatal("Acquire succeeded for a lock file in a missing directory")
	}
	if errors.Is(err, ErrBusy) {
		t.Errorf("got ErrBusy for an unopenable lock file: %v", err)
	}
}
