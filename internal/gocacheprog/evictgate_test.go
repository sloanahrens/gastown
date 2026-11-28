package gocacheprog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

var evictNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// evictLockFree reports whether dir's evict.lock can be taken now.
func evictLockFree(dir string) bool {
	release, err := lock.FlockTryAcquireStable(filepath.Join(dir, "evict.lock"))
	if err != nil {
		return false
	}
	release()
	return true
}

func TestEvictGateSnapshotAge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		missing bool
		age     time.Duration
		run     bool
	}{
		{"missing is due", true, 0, true},
		{"exactly ten minutes is due", false, 10 * time.Minute, true},
		{"one second short is not due", false, 10*time.Minute - time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if !tc.missing {
				snap := filepath.Join(dir, "index.jsonl")
				must(t, os.WriteFile(snap, []byte("{}\n"), 0o644))
				at := evictNow.Add(-tc.age)
				must(t, os.Chtimes(snap, at, at))
			}
			release, run, err := evictGate(dir, evictNow)
			must(t, err)
			if run != tc.run || (release != nil) != run {
				t.Fatalf("run = %v, release nil = %v; want run %v", run, release == nil, tc.run)
			}
			if release != nil {
				release()
			}
			if !evictLockFree(dir) {
				t.Fatal("lock held after the gate returned")
			}
		})
	}
}

func TestEvictGateLockHeldByOtherRefusesWithoutError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	holder, err := lock.FlockTryAcquireStable(filepath.Join(dir, "evict.lock"))
	must(t, err)
	defer holder()
	release, run, err := evictGate(dir, evictNow)
	must(t, err)
	if run || release != nil {
		t.Fatalf("run = %v, release nil = %v; want refusal with no release", run, release == nil)
	}
}

func TestEvictGateSecondCallerRunsOnlyAfterRelease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, run, err := evictGate(dir, evictNow)
	must(t, err)
	if !run {
		t.Fatal("first caller refused")
	}
	if _, run, err := evictGate(dir, evictNow); err != nil || run {
		t.Fatalf("second caller while held: run = %v, err = %v; want refusal", run, err)
	}
	first()
	third, run, err := evictGate(dir, evictNow)
	must(t, err)
	if !run {
		t.Fatal("caller after release refused")
	}
	third()
}

func TestEvictGateLockFailureReturnsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A directory cannot be opened for locking.
	must(t, os.Mkdir(filepath.Join(dir, "evict.lock"), 0o755))
	release, run, err := evictGate(dir, evictNow)
	if err == nil || run || release != nil {
		t.Fatalf("err = %v, run = %v; want error with no release", err, run)
	}
}

func TestEvictGateStatFailureReturnsErrorAndReleasesLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snap := filepath.Join(dir, "index.jsonl")
	// A self-link makes stat fail with ELOOP, not ENOENT.
	must(t, os.Symlink(snap, snap))
	release, run, err := evictGate(dir, evictNow)
	if err == nil || run || release != nil {
		t.Fatalf("err = %v, run = %v; want error with no release", err, run)
	}
	if !evictLockFree(dir) {
		t.Fatal("lock held after stat failure")
	}
}
