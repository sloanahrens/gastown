//go:build unix

package spans

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A wait for the in-process lock gives up at the limit, and the error names the
// lock path. The holder here is another goroutine of this process.
func TestLockLogWithin_GivesUpOnAnotherGoroutineAtTheLimit(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "gt-abc.jsonl.lock")
	held, err := lockLogWithin(lockPath, lockWait)
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	defer held.unlock()

	lk, err := lockLogWithin(lockPath, 100*time.Millisecond)
	if err == nil {
		lk.unlock()
		t.Fatal("lockLogWithin took a lock another goroutine holds, want it to give up at the limit")
	}
	if !strings.Contains(err.Error(), lockPath) || !strings.Contains(err.Error(), "gave up") {
		t.Errorf("error = %v, want it to name the lock path and say it gave up", err)
	}
}

// A wait for a record lock held by another process gives up at the limit, and
// the error names the lock path.
func TestLockLogWithin_GivesUpOnAnotherProcessAtTheLimit(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "gt-abc.jsonl.lock")
	stop := startSpanLockHolder(t, lockPath)
	defer stop()

	lk, err := lockLogWithin(lockPath, 200*time.Millisecond)
	if err == nil {
		lk.unlock()
		t.Fatal("lockLogWithin took a lock another process holds, want it to give up at the limit")
	}
	if !strings.Contains(err.Error(), lockPath) || !strings.Contains(err.Error(), "gave up") {
		t.Errorf("error = %v, want it to name the lock path and say it gave up", err)
	}
}
