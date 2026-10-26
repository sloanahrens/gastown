//go:build unix

package spans

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A wait for the in-process lock gives up at the limit, having waited it, and
// the error names the lock path. The holder here is another goroutine of this
// process.
func TestLockLogWithin_GivesUpOnAnotherGoroutineAtTheLimit(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "gt-abc.jsonl.lock")
	held, err := lockLogWithin(lockPath, lockWait)
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	defer held.unlock()

	const limit = 100 * time.Millisecond
	start := time.Now()
	lk, err := lockLogWithin(lockPath, limit)
	waited := time.Since(start)
	if err == nil {
		lk.unlock()
		t.Fatal("lockLogWithin took a lock another goroutine holds, want it to give up at the limit")
	}
	if !strings.Contains(err.Error(), lockPath) || !strings.Contains(err.Error(), "gave up") {
		t.Errorf("error = %v, want it to name the lock path and say it gave up", err)
	}
	if waited < limit {
		t.Errorf("gave up after %s, want a wait of at least the limit %s", waited, limit)
	}
}

// A wait for a record lock held by another process gives up at the limit,
// having waited it, and the error names the lock path.
func TestLockLogWithin_GivesUpOnAnotherProcessAtTheLimit(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "gt-abc.jsonl.lock")
	stop := startSpanLockHolder(t, lockPath)
	defer stop()

	const limit = 200 * time.Millisecond
	start := time.Now()
	lk, err := lockLogWithin(lockPath, limit)
	waited := time.Since(start)
	if err == nil {
		lk.unlock()
		t.Fatal("lockLogWithin took a lock another process holds, want it to give up at the limit")
	}
	if !strings.Contains(err.Error(), lockPath) || !strings.Contains(err.Error(), "gave up") {
		t.Errorf("error = %v, want it to name the lock path and say it gave up", err)
	}
	if waited < limit {
		t.Errorf("gave up after %s, want a wait of at least the limit %s", waited, limit)
	}
}

// lockLog, the bound every append inherits, is the minute the append waits: no
// test can spend a minute watching it give up, so the value is what this one
// pins.
func TestLockLog_DefaultWaitIsSixtySeconds(t *testing.T) {
	t.Parallel()
	if lockWait != 60*time.Second {
		t.Errorf("lockWait = %s, want the 60-second default lockLog passes to lockLogWithin", lockWait)
	}
}
