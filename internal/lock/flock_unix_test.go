//go:build !windows

package lock

import (
	"errors"
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
