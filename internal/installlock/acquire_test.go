package installlock

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// acquireClock is the clock these tests arm: Sleep advances it, so waiting out
// the lock costs no wall time, and onSleep runs after each Sleep so a test can
// let the other holder go mid-wait.
type acquireClock struct {
	now     time.Time
	slept   []time.Duration
	onSleep func(n int)
}

func newAcquireClock() *acquireClock { return &acquireClock{now: time.Unix(0, 0)} }

func (c *acquireClock) Now() time.Time { return c.now }

func (c *acquireClock) Sleep(d time.Duration) {
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	if c.onSleep != nil {
		c.onSleep(len(c.slept))
	}
}

func (c *acquireClock) waited() time.Duration {
	var sum time.Duration
	for _, d := range c.slept {
		sum += d
	}
	return sum
}

// holdAcquireLock takes the lock through Acquire, as a second install would
// find it held, and returns its release.
func holdAcquireLock(t *testing.T, path string) func() {
	t.Helper()
	release, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("hold %s: %v", path, err)
	}
	return release
}

// acquireFree reports whether a second caller can take the lock on path right
// now: a zero wait takes it when it is free and reports ErrBusy when it is not.
func acquireFree(t *testing.T, path string) bool {
	t.Helper()
	release, err := acquire(path, 0, time.Now, time.Sleep)
	if errors.Is(err, ErrBusy) {
		return false
	}
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	release()
	return true
}

// Acquire holds the lock until its release, the way the Go installer holds it
// from the swap to the end of the run.
func TestAcquireHoldsUntilTheRelease(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")

	release, err := Acquire(lock, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if acquireFree(t, lock) {
		t.Error("the lock was free while Acquire held it")
	}
	release()
	if !acquireFree(t, lock) {
		t.Error("the lock was still held after the release")
	}
}

// A second caller waits the whole wait and then reports ErrBusy, on the
// injected clock so the test spends no wall time.
func TestAcquireReportsBusyWhenTheWaitRunsOut(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")
	release := holdAcquireLock(t, lock)
	defer release()

	clock := newAcquireClock()
	got, err := acquire(lock, 2*time.Second, clock.Now, clock.Sleep)

	if !errors.Is(err, ErrBusy) {
		t.Fatalf("got error %v, want ErrBusy", err)
	}
	if got != nil {
		t.Error("a release was returned for a lock that was never taken")
	}
	if clock.waited() != 2*time.Second {
		t.Errorf("waited %s over %v, want exactly the 2s wait", clock.waited(), clock.slept)
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
	release := holdAcquireLock(t, lock)

	clock := newAcquireClock()
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
	if acquireFree(t, lock) {
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

// The release drops the lock even while another descriptor still holds the
// open file description, the way a child forked mid-hold does until it execs:
// flock belongs to the description, so closing the lock file alone would leave
// the lock held (gt-ubj9e). The test dups the descriptor in place of the fork,
// so the case does not depend on the timing of a real one.
func TestReleaseDropsTheLockWhileADuplicateDescriptorIsOpen(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")

	f, err := acquireFile(lock, time.Second, time.Now, time.Sleep)
	if err != nil {
		t.Fatalf("acquireFile: %v", err)
	}
	dup, err := unix.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	defer func() { _ = unix.Close(dup) }()

	unlockAndClose(f)

	if !acquireFree(t, lock) {
		t.Error("the lock was still held after the release while a duplicate descriptor was open")
	}
}
