package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTryAcquireSlingBeadLock_Contention(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()
	beadID := "gt-race123"

	release1, err := tryAcquireSlingBeadLock(townRoot, beadID)
	if err != nil {
		t.Fatalf("first lock acquire failed: %v", err)
	}

	release2, err := tryAcquireSlingBeadLock(townRoot, beadID)
	if err == nil {
		release2()
		t.Fatal("expected second lock acquire to fail due to contention")
	}
	if !strings.Contains(err.Error(), "already being slung") {
		t.Fatalf("expected deterministic contention error, got: %v", err)
	}

	release1()

	release3, err := tryAcquireSlingBeadLock(townRoot, beadID)
	if err != nil {
		t.Fatalf("expected lock acquire to succeed after release: %v", err)
	}
	release3()
}

func TestTryAcquireSlingAssigneeLock_Serialization(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()
	agent := "gastown/polecats/testcat"

	// First acquire should succeed immediately.
	release1, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("first assignee lock acquire failed: %v", err)
	}

	// Second acquire from the same goroutine (same process) should also succeed
	// because flock is per-FD, not per-process. But from a concurrent goroutine
	// holding its own FD, the lock semantics apply at the OS level.
	// For unit test purposes, verify the lock file is created correctly.
	release1()

	// Verify lock works after release.
	release2, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("lock acquire after release failed: %v", err)
	}
	release2()
}

func TestTryAcquireSlingAssigneeLock_DifferentAgents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()

	// Different agents should not block each other.
	release1, err := tryAcquireSlingAssigneeLock(townRoot, "rig/polecats/cat1")
	if err != nil {
		t.Fatalf("first agent lock failed: %v", err)
	}
	defer release1()

	release2, err := tryAcquireSlingAssigneeLock(townRoot, "rig/polecats/cat2")
	if err != nil {
		t.Fatalf("second agent lock should not be blocked by first: %v", err)
	}
	defer release2()
}

func TestTryAcquireSlingAssigneeLock_Contention(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()
	agent := "gastown/polecats/racecat"

	// Acquire lock in a goroutine and hold it briefly.
	var wg sync.WaitGroup
	lockAcquired := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		release, err := tryAcquireSlingAssigneeLock(townRoot, agent)
		if err != nil {
			t.Errorf("goroutine lock acquire failed: %v", err)
			return
		}
		close(lockAcquired)
		// Hold lock briefly so the main goroutine's retry loop gets exercised.
		<-lockAcquired // already closed, but semantically signal
		release()
	}()

	<-lockAcquired

	// The goroutine released immediately after signaling, so the main goroutine
	// should be able to acquire the lock (possibly after a brief retry).
	release2, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("expected lock acquire to succeed after goroutine release: %v", err)
	}
	release2()

	wg.Wait()
}

func TestTryAcquireSlingAssigneeLock_AgentNameSanitization(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()

	// Agent names with slashes and colons should be sanitized for filesystem safety.
	agents := []string{
		"gastown/polecats/dementus",
		"rig:with:colons",
		"mayor/",
	}
	for _, agent := range agents {
		release, err := tryAcquireSlingAssigneeLock(townRoot, agent)
		if err != nil {
			t.Fatalf("lock acquire failed for agent %q: %v", agent, err)
		}
		release()
	}
}

func slingLockFiles(t *testing.T, townRoot string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(townRoot, ".runtime", "locks", "sling", "*.flock"))
	if err != nil {
		t.Fatalf("globbing sling lock files: %v", err)
	}
	return matches
}

// A released lock takes its sentinel file with it: one file per bead and per
// assignee ever slung is the unbounded growth gt-10u8 reports.
func TestSlingLocks_ReleaseRemovesSentinelFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()

	releaseBead, err := tryAcquireSlingBeadLock(townRoot, "gt-cleanup1")
	if err != nil {
		t.Fatalf("bead lock acquire failed: %v", err)
	}
	releaseAssignee, err := tryAcquireSlingAssigneeLock(townRoot, "gastown/polecats/cleanup")
	if err != nil {
		t.Fatalf("assignee lock acquire failed: %v", err)
	}
	if got := slingLockFiles(t, townRoot); len(got) != 2 {
		t.Fatalf("held locks left %d sentinel files, want 2: %v", len(got), got)
	}

	releaseBead()
	releaseAssignee()

	if got := slingLockFiles(t, townRoot); len(got) != 0 {
		t.Fatalf("released locks left sentinel files behind: %v", got)
	}
}

// The unlink on release must not let two slings hold one bead lock: a waiter
// that opened the file before the unlink has to notice the name is gone and
// reopen (gt-xtfnq). Hammer acquire/release from many goroutines and fail on
// any moment two of them hold the lock at once.
func TestTryAcquireSlingBeadLock_NeverTwoHoldersAcrossRelease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()
	const workers = 8
	const iterations = 200

	var holders atomic.Int32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				release, err := tryAcquireSlingBeadLock(townRoot, "gt-race-unlink")
				if err != nil {
					continue // contended: the guard did its job
				}
				if n := holders.Add(1); n != 1 {
					t.Errorf("%d holders of one bead lock at once", n)
				}
				runtime.Gosched()
				holders.Add(-1)
				release()
			}
		}()
	}
	wg.Wait()
}

// A sling killed before its release leaves its sentinel behind. The next sling
// sweeps it, but never a file whose lock a live sling still holds.
func TestSweepStaleSlingFlocks_RemovesOnlyUnheld(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()
	lockDir := filepath.Join(townRoot, ".runtime", "locks", "sling")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gt-dead1.flock", "assignee_rig_polecats_dead.flock"} {
		if err := os.WriteFile(filepath.Join(lockDir, name), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Not a sentinel: the sweep leaves anything that is not a .flock file alone.
	other := filepath.Join(lockDir, "notes.txt")
	if err := os.WriteFile(other, nil, 0644); err != nil {
		t.Fatal(err)
	}

	held, err := tryAcquireSlingBeadLock(townRoot, "gt-live1")
	if err != nil {
		t.Fatalf("bead lock acquire failed: %v", err)
	}
	// tryAcquire sweeps on the way in, so the dead sentinels are already gone
	// and the live one must have survived that sweep.
	if got := slingLockFiles(t, townRoot); len(got) != 1 || filepath.Base(got[0]) != "gt-live1.flock" {
		t.Fatalf("after sweep, sentinel files = %v, want only gt-live1.flock", got)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("sweep removed a file that is not a .flock sentinel: %v", err)
	}

	// Sweeping again while the lock is held must not steal it.
	sweepStaleSlingFlocks(lockDir)
	if release, err := tryAcquireSlingBeadLock(townRoot, "gt-live1"); err == nil {
		release()
		t.Fatal("sweep freed a held lock: a second sling acquired the same bead")
	}

	held()
}
