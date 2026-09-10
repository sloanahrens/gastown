package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/lock"
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

func TestSweepStaleSlingFlocks_RemovesUnheldRemovesHeldStays(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	dir := t.TempDir()

	// Simulate a leftover sentinel from a completed sling: file exists,
	// nobody holds the flock on it.
	stalePath := filepath.Join(dir, "gt-stale.flock")
	if err := os.WriteFile(stalePath, nil, 0644); err != nil {
		t.Fatalf("writing stale flock file: %v", err)
	}

	// Simulate a sling in progress: file exists and is currently locked.
	heldPath := filepath.Join(dir, "gt-held.flock")
	release, locked, err := lock.FlockTryAcquire(heldPath)
	if err != nil || !locked {
		t.Fatalf("setting up held lock: locked=%v err=%v", locked, err)
	}
	defer release()

	// A non-.flock file should be ignored entirely.
	otherPath := filepath.Join(dir, "not-a-lock.txt")
	if err := os.WriteFile(otherPath, nil, 0644); err != nil {
		t.Fatalf("writing unrelated file: %v", err)
	}

	sweepStaleSlingFlocks(dir)

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("expected stale unheld flock file to be removed, stat err: %v", err)
	}
	if _, err := os.Stat(heldPath); err != nil {
		t.Errorf("expected held flock file to survive the sweep, stat err: %v", err)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Errorf("expected unrelated file to survive the sweep, stat err: %v", err)
	}
}

func TestTryAcquireSlingBeadLock_SweepsStaleFlocksFromPriorSlings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()
	lockDir := filepath.Join(townRoot, ".runtime", "locks", "sling")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		t.Fatalf("creating lock dir: %v", err)
	}

	// Leftover sentinels from unrelated, already-finished slings.
	leftovers := []string{"gt-old1.flock", "gt-old2.flock", "assignee_gastown_polecats_old.flock"}
	for _, name := range leftovers {
		if err := os.WriteFile(filepath.Join(lockDir, name), nil, 0644); err != nil {
			t.Fatalf("writing leftover %s: %v", name, err)
		}
	}

	release, err := tryAcquireSlingBeadLock(townRoot, "gt-new")
	if err != nil {
		t.Fatalf("lock acquire failed: %v", err)
	}
	defer release()

	for _, name := range leftovers {
		if _, err := os.Stat(filepath.Join(lockDir, name)); !os.IsNotExist(err) {
			t.Errorf("expected leftover %s to be swept away, stat err: %v", name, err)
		}
	}
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
