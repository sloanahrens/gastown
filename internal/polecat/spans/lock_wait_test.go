//go:build unix

package spans

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
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

// The default limit bounds an append: a contended lock given lockWait waits
// the whole of it, and only then gives up, naming the path. The fake clock
// passes the minute without spending it.
func TestLockLogAt_GivesUpAtTheDefaultWait(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "gt-abc.jsonl.lock")
	held, err := lockLog(lockPath)
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	defer held.unlock()

	clk := clockwork.NewFakeClockAt(time.Unix(0, 0))
	gaveUp := make(chan error, 1)
	go func() {
		_, err := lockLogAt(lockPath, lockWait, clk)
		gaveUp <- err
	}()

	// The clock is advanced only once the waiter holds its deadline timer, so
	// the limit ends the wait rather than an advance made too early. The
	// context is a deadlock guard, not a wait: a waiter that never starts the
	// timer fails the test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := clk.BlockUntilContext(ctx, 1); err != nil {
		t.Fatalf("waiting for the append to start its deadline timer: %v", err)
	}

	clk.Advance(lockWait - time.Nanosecond)
	select {
	case <-gaveUp:
		t.Fatalf("gave up after %s, want a wait of the whole limit %s", lockWait-time.Nanosecond, lockWait)
	default:
	}

	clk.Advance(time.Nanosecond)
	err = <-gaveUp
	if err == nil {
		t.Fatal("lockLogAt took a lock another goroutine holds, want it to give up at the limit")
	}
	if !strings.Contains(err.Error(), lockPath) || !strings.Contains(err.Error(), "gave up") {
		t.Errorf("error = %v, want it to name the lock path and say it gave up", err)
	}
}
