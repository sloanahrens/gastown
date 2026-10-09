//go:build !windows

package slot

import (
	"os"
	"syscall"
	"testing"

	"github.com/steveyegge/gastown/internal/lock"
)

// TestStaleOwnerRemovalHoldsTheSlotFlock: a stale owner file must be removed
// while the slot's flock is still held. Releasing it first opens a window in
// which the next acquirer takes the slot and publishes its own owner file,
// which the removal then deletes — a live holder with no owner file for the
// gate and the full-suite cap to read (gt-u0zq0).
//
// The stale file is a FIFO, which parks the sweep inside its own removal path —
// after the flock probe, before the unlink — until this test opens the write
// end. At that instant the test probes the slot's flock: a sweep that still
// holds it fails the probe, and one that has already released it takes the lock
// and the assertion below fails.
func TestStaleOwnerRemovalHoldsTheSlotFlock(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	lockPath := SlotLockPath(townRoot, 0)
	if err := os.WriteFile(lockPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	ownerPath := SlotOwnerPath(townRoot, 0)
	if err := syscall.Mkfifo(ownerPath, 0644); err != nil {
		t.Fatalf("mkfifo %s: %v", ownerPath, err)
	}

	swept := make(chan struct{})
	go func() {
		defer close(swept)
		reapStaleOwnerFiles(townRoot, false)
	}()

	// Opening the write end completes only when the sweep opens the read end,
	// so by the next line the sweep is blocked between its flock probe and its
	// unlink.
	w, err := os.OpenFile(ownerPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening %s for writing: %v", ownerPath, err)
	}
	unlock, held, err := lock.FlockTryAcquire(lockPath)
	if err != nil {
		t.Fatalf("probing %s: %v", lockPath, err)
	}
	if held {
		unlock()
	}

	// Unblock the sweep whatever the verdict, so its goroutine finishes.
	if _, err := w.Write(claimJSON("dead", 999999, "")); err != nil {
		t.Fatalf("writing to %s: %v", ownerPath, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing %s: %v", ownerPath, err)
	}
	<-swept

	if held {
		t.Fatal("the stale owner file was removed after the slot's flock was released: an acquirer in that window loses the file it just wrote")
	}
}
