//go:build !windows

package lock

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestFlockTryAcquireStableRetriesAnUnlinkedInode reproduces the race a holder
// that unlinks its lock file before letting the flock go opens up: a waiter
// whose open landed before the unlink locks the inode the unlink left nameless,
// so a second process opening the path takes a different file — two holders of
// one lock (gt-xtfnq). The stable acquire must reject the nameless descriptor
// and start over, leaving the path naming the file it locked.
func TestFlockTryAcquireStableRetriesAnUnlinkedInode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "marker.lock")

	// A peer's descriptor from before the unlink: opened, then the name removed
	// out from under it, exactly as MarkerHandle.Release does.
	stale, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("opening the descriptor about to be stranded: %v", err)
	}
	defer stale.Close() //nolint:errcheck // the acquire closes it too
	if err := os.Remove(path); err != nil {
		t.Fatalf("unlinking the stranded descriptor's file: %v", err)
	}

	opens := 0
	unlock, err := flockTryAcquireStable(func() (*os.File, error) {
		opens++
		if opens == 1 {
			return stale, nil
		}
		return flockOpenFile(path)
	})
	if err != nil {
		t.Fatalf("stable acquire = %v, want the lock", err)
	}
	defer unlock()

	// Accepting the nameless descriptor would leave the path unlocked, so this
	// second acquire would succeed — the two-holders bug.
	if _, err := FlockTryAcquireStable(path); !errors.Is(err, ErrFlockHeld) {
		t.Fatalf("second acquire = %v, want ErrFlockHeld: the first lock must be on the file the path names", err)
	}
}

// TestFlockTryAcquireExistingNeverCreatesTheFile: a read-only probe must be
// able to ask whether a lock is taken without leaving a lock file behind, and
// must report a lock someone else holds as held rather than as free.
func TestFlockTryAcquireExistingNeverCreatesTheFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gate.lock")

	if _, err := FlockTryAcquireExisting(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("probing an absent lock file = %v; want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the read-only probe left %s behind (stat err %v)", path, err)
	}

	held, took, err := FlockTryAcquire(path)
	if err != nil || !took {
		t.Fatalf("holding the lock = (%v, %v); want it taken", took, err)
	}

	if unlock, err := FlockTryAcquireExisting(path); !errors.Is(err, ErrFlockHeld) {
		t.Fatalf("probing a held lock = %v; want ErrFlockHeld", err)
	} else if unlock != nil {
		t.Errorf("probing a held lock returned a cleanup; nothing was taken")
	}

	held()

	unlock, err := FlockTryAcquireExisting(path)
	if err != nil {
		t.Fatalf("probing a released lock = %v; want the lock", err)
	}
	unlock()
}
