package slot

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// settleFinalizers runs the garbage collector until the finalizer goroutine has
// worked through everything that became unreachable before the call: each
// round queues a sentinel and waits for it to run, and three rounds cover a
// finalizer queued behind the sentinel of the round before.
func settleFinalizers(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for round := 0; round < 3; round++ {
		ran := make(chan struct{})
		sentinel := new([16]byte)
		runtime.SetFinalizer(sentinel, func(*[16]byte) { close(ran) })
		sentinel = nil
		for done := false; !done; {
			runtime.GC()
			select {
			case <-ran:
				done = true
			case <-ctx.Done():
				t.Fatal("finalizers never ran")
			default:
				runtime.Gosched()
			}
		}
	}
}

// acquireAndDrop takes a slot and a marker and lets go of both handles
// without releasing them.
func acquireAndDrop(t *testing.T, tg *testGate, town string) {
	t.Helper()
	// A free slot is granted on the first pass, before the poll loop ever
	// sleeps, so this needs no clock driving.
	if _, err := tg.Acquire(town, "gastown/landing", time.Second); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := tg.AcquireMarker(town, testMarkerName, "gastown/om-review"); err != nil {
		t.Fatalf("AcquireMarker: %v", err)
	}
}

// TestDroppedHandleKeepsItsSlot: only Release (or the holder's death) ends a
// hold. A Handle its holder stops referencing must not be garbage collected
// into releasing the flock, or a second suite is admitted beside the first
// with nothing on record (gt-1okjo).
func TestDroppedHandleKeepsItsSlot(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()

	acquireAndDrop(t, tg, town)
	settleFinalizers(t)

	for _, path := range []string{SlotLockPath(town, 0), MarkerLockPath(town, testMarkerName)} {
		unlock, took, err := lock.FlockTryAcquire(path)
		if err != nil {
			t.Fatalf("probing %s: %v", path, err)
		}
		if took {
			unlock()
			t.Errorf("%s was released by the garbage collector while its holder never called Release", path)
		}
	}
}

// TestPinHold: the pinned release unlocks once however often it is called,
// and lets go of the hold, so a released hold is collectable again rather than
// accumulating in the registry for the life of the process.
func TestPinHold(t *testing.T) {
	t.Parallel()
	unlocks := 0
	collected := make(chan struct{})
	release := pinHold(fakeUnlock(&unlocks, collected))

	release()
	release()
	if unlocks != 1 {
		t.Errorf("unlock ran %d times, want once", unlocks)
	}

	release = nil
	settleFinalizers(t)
	select {
	case <-collected:
	default:
		t.Error("a released hold is still reachable from the registry")
	}
}

// fakeUnlock returns an unlock func that owns a stand-in for the open lock
// file, which signals collected once nothing references the func any more.
func fakeUnlock(unlocks *int, collected chan struct{}) func() {
	lockFile := new([16]byte)
	runtime.SetFinalizer(lockFile, func(*[16]byte) { close(collected) })
	return func() {
		*unlocks++
		runtime.KeepAlive(lockFile)
	}
}
