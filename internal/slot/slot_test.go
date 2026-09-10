package slot

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireReleaseStatus(t *testing.T) {
	townRoot := t.TempDir()

	held, owner, err := Status(townRoot)
	if err != nil {
		t.Fatalf("Status before acquire: %v", err)
	}
	if held {
		t.Fatalf("Status reported held before any Acquire; owner=%+v", owner)
	}

	h, err := Acquire(townRoot, "test-role", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	held, owner, err = Status(townRoot)
	if err != nil {
		t.Fatalf("Status after acquire: %v", err)
	}
	if !held {
		t.Fatalf("Status reported free while held")
	}
	if owner == nil || owner.Role != "test-role" || owner.PID != os.Getpid() {
		t.Fatalf("owner metadata wrong: %+v", owner)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	held, _, err = Status(townRoot)
	if err != nil {
		t.Fatalf("Status after release: %v", err)
	}
	if held {
		t.Fatalf("Status reported held after Release")
	}

	if _, err := os.Stat(OwnerPath(townRoot)); !os.IsNotExist(err) {
		t.Fatalf("owner file should be removed after Release, stat err=%v", err)
	}
}

// TestAcquire_MutualExclusion asserts the slot cannot be double-held: a
// second Acquire on an already-held slot must time out, and the exact wait
// window is a count (elapsed >= timeout), not merely "eventually returns an
// error" — a timeout implementation that returns immediately would also
// satisfy a looser assertion without actually enforcing exclusion.
func TestAcquire_MutualExclusion(t *testing.T) {
	townRoot := t.TempDir()

	h, err := Acquire(townRoot, "holder", time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer h.Release()

	timeout := DefaultPollInterval + 500*time.Millisecond
	start := time.Now()
	_, err = Acquire(townRoot, "waiter", timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("second Acquire on a held slot succeeded; mutual exclusion broken")
	}
	if elapsed < timeout {
		t.Fatalf("second Acquire returned after %s, before its %s timeout elapsed — it did not actually wait/retry", elapsed, timeout)
	}
}

// TestAcquire_ReleaseUnblocksWaiter is the mutual-exclusion test's
// complement: it proves a released slot is genuinely acquirable again, not
// just that a held one is unavailable.
func TestAcquire_ReleaseUnblocksWaiter(t *testing.T) {
	townRoot := t.TempDir()

	h, err := Acquire(townRoot, "holder", time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		h2, err := Acquire(townRoot, "waiter", 5*time.Second)
		if err != nil {
			done <- err
			return
		}
		done <- h2.Release()
	}()

	// Give the waiter a couple of poll cycles to observe the held lock
	// before releasing, so this actually exercises the wait path.
	time.Sleep(DefaultPollInterval + 200*time.Millisecond)
	if err := h.Release(); err != nil {
		t.Fatalf("releasing holder: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter Acquire/Release after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never acquired the slot after it was released")
	}
}

// TestAcquire_KernelReleasesOnProcessDeath is the adversarial check for the
// mechanism this package exists to provide: the whole point of using
// flock(2) instead of a hand-rolled pid/mkdir lock (see the gt-bcsq
// discussion of v1-v2.3) is that the KERNEL releases the lock when the
// holding process dies by any means, including SIGKILL, with no reclaim
// logic to get wrong. A regression that swapped the real lock file for e.g.
// a plain "does this path exist" check would still pass every
// acquire/release/status assertion above yet fail this one, because nothing
// would ever remove the file after a SIGKILL.
func TestAcquire_KernelReleasesOnProcessDeath(t *testing.T) {
	townRoot := t.TempDir()
	lockPath := LockPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		t.Fatal(err)
	}

	bin, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve test binary: %v", err)
	}

	cmd := exec.Command(bin, "-test.run=TestHelperHoldSlotUntilKilled")
	cmd.Env = append(os.Environ(), "GT_SLOT_HELPER=1", "GT_SLOT_TOWN_ROOT="+townRoot)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper process: %v", err)
	}

	// Wait for the helper to actually acquire the slot before killing it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		held, _, err := Status(townRoot)
		if err != nil {
			t.Fatalf("Status while waiting for helper: %v", err)
		}
		if held {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("helper process never acquired the slot")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := cmd.Process.Kill(); err != nil { // SIGKILL — no cleanup handlers run
		t.Fatalf("killing helper: %v", err)
	}
	_ = cmd.Wait()

	// The kernel must release the flock the instant the process's file
	// descriptors close, with no timeout or reclaim step required.
	h2, err := Acquire(townRoot, "post-kill", 3*time.Second)
	if err != nil {
		t.Fatalf("slot still held after SIGKILLing the holder: %v", err)
	}
	_ = h2.Release()
}

// TestHelperHoldSlotUntilKilled is not a real test; it is spawned as a
// subprocess by TestAcquire_KernelReleasesOnProcessDeath to hold the slot
// until SIGKILLed, with no Release() call in its shutdown path.
func TestHelperHoldSlotUntilKilled(t *testing.T) {
	if os.Getenv("GT_SLOT_HELPER") != "1" {
		t.Skip("not invoked as slot-holder helper")
	}
	townRoot := os.Getenv("GT_SLOT_TOWN_ROOT")
	if _, err := Acquire(townRoot, "helper", time.Second); err != nil {
		t.Fatalf("helper failed to acquire: %v", err)
	}
	time.Sleep(30 * time.Second) // outlived by the parent's SIGKILL
}
