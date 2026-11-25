package polecat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/rig"
)

// Two seat picks on one rig must not run at once (gt-0igsxt). The test proves the
// lock by its state, not by what a second goroutine does in time: seatPickLockFree
// opens seat-pick.lock on a descriptor of its own, and a flock is held per open
// file description, so while a pick holds the lock the probe is refused, and after
// the unlock it is granted. That a blocking flock on a held lock waits is the
// kernel's contract, not this package's to test. The assertions run in one
// goroutine, in order, with no yield and no wait deciding them; a LockSeatPick that
// does not take the lock leaves the probe free and fails the first assertion on
// every run, at any GOMAXPROCS (gt-jtxwd7).
func TestLockSeatPickBlocksASecondHolderOnTheSameRigUntilUnlock(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	m := newTestManager(&rig.Rig{Name: "myrig", Path: rigPath}, newWorld(), nil, newNoDatabaseDB())
	// assertFree reports the failure with Errorf, because it runs off the test
	// goroutine, and says whether the step held.
	assertFree := func(step string, want bool) bool {
		t.Helper()
		free, err := seatPickLockFree(rigPath)
		if err != nil {
			t.Errorf("%s: probing seat-pick.lock: %v", step, err)
			return false
		}
		if free != want {
			t.Errorf("%s: seat-pick.lock free = %v, want %v", step, free, want)
			return false
		}
		return true
	}
	// The picks run in one goroutine under a deadline that only fails a stuck run:
	// a second pick that never returns would otherwise hang the package.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		unlock, err := m.LockSeatPick()
		if err != nil {
			t.Errorf("first LockSeatPick: %v", err)
			return
		}
		held := assertFree("while the first pick holds the lock", false)
		unlock()
		if !held || !assertFree("after the first pick unlocked", true) {
			return
		}
		second, err := m.LockSeatPick()
		if err != nil {
			t.Errorf("second LockSeatPick after the first unlock: %v", err)
			return
		}
		held = assertFree("while the second pick holds the lock", false)
		second()
		if held {
			assertFree("after the second pick unlocked", true)
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the seat picks did not finish: a pick is stuck")
	}
}

func TestLockSeatPickOnAnotherRigDoesNotWait(t *testing.T) {
	t.Parallel()
	unlock, err := newTestManager(&rig.Rig{Name: "a", Path: t.TempDir()}, newWorld(), nil, newNoDatabaseDB()).LockSeatPick()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	other, err := newTestManager(&rig.Rig{Name: "b", Path: t.TempDir()}, newWorld(), nil, newNoDatabaseDB()).LockSeatPick()
	if err != nil {
		t.Fatalf("LockSeatPick on another rig: %v", err)
	}
	other()
}

// The seam runs once per LockSeatPick, after the lock directory exists and
// before the flock is taken, so the lock is still free when it runs (gt-mn6lr9).
func TestLockSeatPickSeamRunsOnceBeforeTheLockIsTaken(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	m := newTestManager(&rig.Rig{Name: "myrig", Path: rigPath}, newWorld(), nil, newNoDatabaseDB())
	var calls int
	var freeAtCall bool
	var probeErr error
	m.seatPickAttempt = func() {
		calls++
		freeAtCall, probeErr = seatPickLockFree(rigPath)
	}
	unlock, err := m.LockSeatPick()
	if err != nil {
		t.Fatalf("LockSeatPick: %v", err)
	}
	defer unlock()
	if calls != 1 {
		t.Fatalf("seam ran %d times, want 1", calls)
	}
	if probeErr != nil {
		t.Fatalf("probing the seat-pick lock from the seam: %v", probeErr)
	}
	if !freeAtCall {
		t.Fatal("seam ran while the seat-pick lock was already held")
	}
}

// With no seam set, LockSeatPick takes the lock and releases it as before (gt-mn6lr9).
func TestLockSeatPickSeamNilTakesTheLock(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	m := newTestManager(&rig.Rig{Name: "myrig", Path: rigPath}, newWorld(), nil, newNoDatabaseDB())
	unlock, err := m.LockSeatPick()
	if err != nil {
		t.Fatalf("LockSeatPick with no seam: %v", err)
	}
	if free, err := seatPickLockFree(rigPath); err != nil || free {
		t.Fatalf("lock not held after LockSeatPick: free %v, err %v", free, err)
	}
	unlock()
	if free, err := seatPickLockFree(rigPath); err != nil || !free {
		t.Fatalf("lock not free after unlock: free %v, err %v", free, err)
	}
}

// LockSeatPick refuses, rather than returning a lock it does not hold, when the lock cannot be opened.
func TestLockSeatPickRefusesWhenTheLockCannotBeTaken(t *testing.T) {
	t.Parallel()
	tests := map[string]func(p string) error{
		"lock dir cannot be created": func(p string) error { return os.WriteFile(filepath.Join(p, ".runtime"), nil, 0o644) },
		"lock path is a directory": func(p string) error {
			return os.MkdirAll(filepath.Join(p, ".runtime", "locks", "seat-pick.lock"), 0o755)
		},
	}
	for name, setup := range tests {
		rigPath := t.TempDir()
		if err := setup(rigPath); err != nil {
			t.Fatal(err)
		}
		if unlock, err := newTestManager(&rig.Rig{Name: "myrig", Path: rigPath}, newWorld(), nil, newNoDatabaseDB()).LockSeatPick(); err == nil {
			unlock()
			t.Errorf("%s: LockSeatPick returned no error", name)
		}
	}
}

func seatPickLockFree(rigPath string) (bool, error) {
	probe := flock.New(filepath.Join(rigPath, ".runtime", "locks", "seat-pick.lock"))
	ok, err := probe.TryLock()
	if ok {
		err = probe.Unlock()
	}
	return ok, err
}
