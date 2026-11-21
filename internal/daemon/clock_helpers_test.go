package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// testEpoch is the fixed start of every fake clock in this package.
var testEpoch = time.Date(2026, time.January, 2, 15, 4, 5, 0, time.UTC)

// newFixedClock returns a fake clock at testEpoch.
func newFixedClock() *clockwork.FakeClock {
	return clockwork.NewFakeClockAt(testEpoch)
}

// driftSpyClock is the drift tests' fake clock, which counts the timers the
// reconcile creates. A test uses the count when it must know the loop has
// re-armed its tick after a push: the old tick and the new one each hold one
// clock wait, so waiting on the waits cannot tell them apart.
type driftSpyClock struct {
	*clockwork.FakeClock
	timers atomic.Int64
	armed  chan struct{}
}

// newDriftSpyClock returns a spy clock at testEpoch.
func newDriftSpyClock() *driftSpyClock {
	return &driftSpyClock{FakeClock: newFixedClock(), armed: make(chan struct{}, 16)}
}

// NewTimer creates the timer through the embedded fake clock and records it.
// The send only wakes awaitDriftTimers: the count is the record, so a send the
// buffer refuses loses nothing as long as the waiter re-reads the count.
func (c *driftSpyClock) NewTimer(d time.Duration) clockwork.Timer {
	t := c.FakeClock.NewTimer(d)
	c.timers.Add(1)
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return t
}

// awaitDriftTimers waits, under the hang guard, until the reconcile has created
// n timers on c.
func awaitDriftTimers(t *testing.T, c *driftSpyClock, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), workspaceHangGuard)
	defer cancel()
	for c.timers.Load() < int64(n) {
		select {
		case <-c.armed:
		case <-ctx.Done():
			t.Fatalf("the reconcile created %d timers within %s, want %d", c.timers.Load(), workspaceHangGuard, n)
		}
	}
}

// driveClock advances clk by step every time a goroutine blocks on it, until
// done delivers a value. It fails the test if nothing blocks within 10 s.
func driveClock[T any](t *testing.T, clk *clockwork.FakeClock, step time.Duration, done <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		blocked := make(chan error, 1)
		go func() { blocked <- clk.BlockUntilContext(ctx, 1) }()
		select {
		case v := <-done:
			return v
		case err := <-blocked:
			if err != nil {
				select {
				case v := <-done:
					return v
				default:
				}
				t.Fatalf("nothing blocked on the fake clock: %v", err)
			}
			clk.Advance(step)
		}
	}
}

// runOnClock runs f on its own goroutine and drives clk (see driveClock) until
// f returns. It suits code under test that sleeps through the daemon's clock
// one sleeper at a time.
func runOnClock(t *testing.T, clk *clockwork.FakeClock, step time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{}, 1)
	go func() {
		defer func() { done <- struct{}{} }()
		f()
	}()
	driveClock(t, clk, step, done)
}

// lockedBuffer is a bytes.Buffer safe to write from the daemon's goroutines
// while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// discardLogger is what a test reaches for when it needs a Daemon.logger but
// never inspects what it writes. os.Stderr is the process-wide fd go test
// streams, so fixture text logged there landed in gate logs beside real
// output (gt-tw45); io.Discard touches no fd.
var discardLogger = log.New(io.Discard, "", 0)
