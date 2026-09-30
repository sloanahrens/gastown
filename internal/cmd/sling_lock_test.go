package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// A held assignee lock is waited for, not refused: the retry sleeps between
// attempts and takes the lock once the holder lets go. The wait is injected, so
// the holder's release happens inside the first sleep rather than racing it.
func TestTryAcquireSlingAssigneeLock_WaitsForTheHolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()
	agent := "gastown/polecats/racecat"
	holder, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("holder lock acquire failed: %v", err)
	}

	var sleeps []time.Duration
	release, err := tryAcquireSlingAssigneeLockWith(townRoot, agent, func(d time.Duration) {
		sleeps = append(sleeps, d)
		holder()
	})
	if err != nil {
		t.Fatalf("the waiter must take the lock once the holder releases: %v", err)
	}
	release()
	if len(sleeps) != 1 || sleeps[0] != 500*time.Millisecond {
		t.Errorf("sleeps = %v, want one 500ms wait before the retry", sleeps)
	}
}

// A holder that never lets go is a stuck sling: the waiter gives up after its
// bounded retries and says so, rather than blocking the sling forever.
func TestTryAcquireSlingAssigneeLock_TimesOutOnAStuckHolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}
	t.Parallel()

	townRoot := t.TempDir()
	agent := "gastown/polecats/stuckcat"
	holder, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("holder lock acquire failed: %v", err)
	}
	defer holder()

	sleeps := 0
	release, err := tryAcquireSlingAssigneeLockWith(townRoot, agent, func(time.Duration) { sleeps++ })
	if err == nil {
		release()
		t.Fatal("a lock held throughout must not be acquired")
	}
	if !strings.Contains(err.Error(), "timed out acquiring assignee sling lock for "+agent+" after 10s") {
		t.Errorf("error = %v, want the timeout named", err)
	}
	if sleeps != 19 {
		t.Errorf("slept %d times, want 19 (a wait between each of 20 attempts)", sleeps)
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

// The unlink on release must not let two slings hold one bead lock: the name
// goes while the flock is still held, so a waiter that opened the file before
// the unlink finds the name gone and reopens (gt-xtfnq; the reopen itself is
// pinned in internal/lock). Unlocking first would let a waiter lock the old
// inode and then lose its name to this unlink.
func TestUnlinkThenUnlock_RemovesTheNameBeforeTheLockDrops(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gt-race-unlink.flock")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	unlocked := false
	unlinkThenUnlock(path, func() {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the lock dropped while its name still existed (stat err %v)", err)
		}
		unlocked = true
	})()
	if !unlocked {
		t.Error("release never dropped the lock")
	}
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
