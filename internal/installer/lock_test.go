package installer

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/installlock"
	"github.com/steveyegge/gastown/internal/lock"
)

// fakeLock is the Acquire seam: it records every call and the lock it is asked
// for, and keeps held true between acquire and release so a callback can see
// whether the install lock was held when it ran.
type fakeLock struct {
	calls    int
	wait     time.Duration
	held     bool
	releases int
	err      error
}

func (f *fakeLock) acquire(_ string, wait time.Duration) (func(), error) {
	f.calls++
	f.wait = wait
	if f.err != nil {
		return nil, f.err
	}
	f.held = true
	return func() {
		f.held = false
		f.releases++
	}, nil
}

func noop() Exit { return ExitOK }

// The lock wait a run uses when it sets none is 300 seconds: an install queued
// behind a build must not wait out the build, because the build holds only its
// own lock.
func TestLockWaitDefaultsTo300Seconds(t *testing.T) {
	t.Parallel()
	f := &fakeLock{}
	c := &Contract{Stdout: io.Discard}

	c.Swap(InstallLock{Path: "/daemon/install-gt.lock", Acquire: f.acquire}, noop, noop)

	if f.wait != 300*time.Second {
		t.Errorf("waited %s for the lock, want 300s", f.wait)
	}
}

// A run that injects no Acquire takes the real flock, and drops it at the end.
func TestTheDefaultAcquireTakesTheRealFlock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "install-gt.lock")
	var heldDuringSwap bool
	c := &Contract{Stdout: io.Discard}

	code := c.Swap(InstallLock{Path: path},
		noop,
		func() Exit {
			release, free, err := lock.FlockTryAcquire(path)
			if err != nil {
				t.Fatalf("probe the install lock: %v", err)
			}
			heldDuringSwap = !free
			if free {
				release()
			}
			return ExitOK
		})

	if code != ExitOK {
		t.Errorf("Swap returned %d, want %d", code, ExitOK)
	}
	if !heldDuringSwap {
		t.Error("the swap ran without the real install lock taken")
	}
	release, free, err := lock.FlockTryAcquire(path)
	if err != nil {
		t.Fatalf("probe the install lock after the run: %v", err)
	}
	if free {
		release()
	} else {
		t.Error("the real install lock was still held after the run")
	}
}

// The image build runs with no install lock held, the swap runs under it, and
// the lock is released once guarded returns.
func TestImageBuildHoldsNoInstallLockAndTheSwapDoes(t *testing.T) {
	t.Parallel()
	f := &fakeLock{}
	var imageCalls int
	var imageSawLock bool
	var swapSawLock bool
	c := &Contract{Stdout: io.Discard}

	code := c.Swap(InstallLock{Path: "/daemon/install-gt.lock", Acquire: f.acquire},
		func() Exit {
			imageCalls++
			imageSawLock = f.held
			return ExitOK
		},
		func() Exit {
			swapSawLock = f.held
			return ExitOK
		})

	if code != ExitOK {
		t.Errorf("Swap returned %d, want %d", code, ExitOK)
	}
	if imageCalls != 1 {
		t.Errorf("the image build ran %d times, want 1", imageCalls)
	}
	if imageSawLock {
		t.Error("the image build ran with the install lock held")
	}
	if !swapSawLock {
		t.Error("the swap ran with the install lock not held")
	}
	if f.held {
		t.Error("the install lock was still held after the guarded step")
	}
	if f.releases != 1 {
		t.Errorf("released the lock %d times, want 1", f.releases)
	}
}

// A failed image build stops the run before the lock is taken: nothing was
// built, so there is nothing to swap and no reason for another install to wait.
func TestFailedImageBuildTakesNoLock(t *testing.T) {
	t.Parallel()
	f := &fakeLock{}
	swapRan := false
	c := &Contract{Stdout: io.Discard}

	code := c.Swap(InstallLock{Path: "/daemon/install-gt.lock", Acquire: f.acquire},
		func() Exit { return ExitFailed },
		func() Exit { swapRan = true; return ExitOK })

	if code != ExitFailed {
		t.Errorf("Swap returned %d, want %d", code, ExitFailed)
	}
	if f.calls != 0 {
		t.Error("the install lock was taken though the image build failed")
	}
	if swapRan {
		t.Error("the swap ran though the image build failed")
	}
}

// An install that finds the lock held waits its wait through the real flock and
// is then refused busy: the swap never runs.
func TestAnInstallBehindAHeldLockIsRefusedBusy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "install-gt.lock")
	release, free, err := lock.FlockTryAcquire(path)
	if err != nil || !free {
		t.Fatalf("hold the install lock: free=%v, err=%v", free, err)
	}
	defer release()
	var out strings.Builder
	swapRan := false
	c := &Contract{Stdout: &out, Expected: "abc123"}

	code := c.Swap(InstallLock{Path: path, Wait: 20 * time.Millisecond},
		noop,
		func() Exit { swapRan = true; return ExitOK })

	if code != ExitBusy {
		t.Errorf("Swap returned %d, want %d", code, ExitBusy)
	}
	if want := "install-gt: RESULT refused abc123 - lock-busy\n"; !strings.HasSuffix(out.String(), want) {
		t.Errorf("wrote %q, want it to end with %q", out.String(), want)
	}
	if swapRan {
		t.Error("the swap ran though the lock was never taken")
	}
}

// An install that waits out the lock is refused busy in the script's line and
// status, and the swap does not run.
func TestInstallThatWaitsOutTheLockIsRefusedBusy(t *testing.T) {
	t.Parallel()
	receipts := filepath.Join(t.TempDir(), "install-receipts.jsonl")
	var out strings.Builder
	f := &fakeLock{err: installlock.ErrBusy}
	swapRan := false
	c := &Contract{Stdout: &out, Receipts: receipts, Expected: "abc123", Source: "manual"}

	code := c.Swap(InstallLock{Path: "/daemon/install-gt.lock", Wait: 20 * time.Millisecond, Acquire: f.acquire},
		noop,
		func() Exit { swapRan = true; return ExitOK })

	if code != ExitBusy {
		t.Errorf("Swap returned %d, want %d", code, ExitBusy)
	}
	if f.wait != 20*time.Millisecond {
		t.Errorf("waited %s for the lock, want the configured 20ms", f.wait)
	}
	want := "[install-gt] Another install held the lock for 20ms; not installing abc123.\n" +
		"install-gt: RESULT refused abc123 - lock-busy\n"
	if got := out.String(); got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
	if swapRan {
		t.Error("the swap ran though the lock was never taken")
	}
	rec, err := os.ReadFile(receipts)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"event": "refused"`, `"commit": "abc123"`, `"reason": "lock-busy"`} {
		if !strings.Contains(string(rec), field) {
			t.Errorf("receipt %q does not carry %s", rec, field)
		}
	}
}

// A lock that cannot be taken at all is a failed run, not a busy one: the
// caller must not read it as another install holding the lock.
func TestLockThatCannotBeTakenFailsTheRun(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	f := &fakeLock{err: errors.New("permission denied")}
	c := &Contract{Stdout: &out}

	code := c.Swap(InstallLock{Path: "/daemon/install-gt.lock", Acquire: f.acquire}, noop, noop)

	if code != ExitFailed {
		t.Errorf("Swap returned %d, want %d", code, ExitFailed)
	}
	if want := "[install-gt] Cannot take the install lock: permission denied\n"; out.String() != want {
		t.Errorf("wrote %q, want %q", out.String(), want)
	}
}
