package daemon

import (
	"context"
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
