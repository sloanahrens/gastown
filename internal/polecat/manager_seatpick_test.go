package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/rig"
)

// Two seat picks on one rig must not run at once (gt-0igsxt).
func TestLockSeatPickBlocksASecondHolderOnTheSameRigUntilUnlock(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	m := newTestManager(&rig.Rig{Name: "myrig", Path: rigPath}, newWorld(), nil, newNoDatabaseDB())
	unlock, err := m.LockSeatPick()
	if err != nil {
		t.Fatalf("first LockSeatPick: %v", err)
	}
	acquired := make(chan error, 1)
	go func() {
		second, err := m.LockSeatPick()
		if err == nil {
			defer second()
			var free bool
			free, err = seatPickLockFree(rigPath)
			if err == nil && free {
				err = errors.New("second LockSeatPick returned without holding seat-pick.lock")
			}
		}
		acquired <- err
	}()

	if ok, err := awaitResult(acquired); ok {
		t.Fatalf("second LockSeatPick returned while the first pick held the lock (err %v)", err)
	}
	unlock()
	if ok, err := awaitResult(acquired); !ok || err != nil {
		t.Fatalf("second LockSeatPick after the first unlock: returned %v, err %v", ok, err)
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

// awaitResult returns the next result on ch, or false after a bounded number of yields.
func awaitResult(ch <-chan error) (bool, error) {
	for i := 0; i < 100000; i++ {
		select {
		case err := <-ch:
			return true, err
		default:
			runtime.Gosched()
		}
	}
	return false, nil
}
