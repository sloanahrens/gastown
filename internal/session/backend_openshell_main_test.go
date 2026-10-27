package session

import (
	"slices"
	"testing"
	"time"
)

// The exit poll is judged here, not in the integration tier: waitForExit takes
// its clock as a seam, so these tests start nothing and pass no real time.

// waitForExit reports whether exited reports true within timeout. It asks
// exited at each interval until it does or the deadline passes. The last sleep
// is shortened to what the deadline leaves, so a process that never goes away
// ends the poll at the deadline rather than a fixed step past it. now and sleep
// are seams: the boundary tests drive the deadline with a fake clock, so no
// real time passes and every sleep is accounted for.
func waitForExit(exited func() bool, timeout time.Duration, now func() time.Time, sleep func(time.Duration)) bool {
	deadline := now().Add(timeout)
	for {
		if exited() {
			return true
		}
		if !now().Before(deadline) {
			return false
		}
		pause := exitPollInterval
		if remaining := deadline.Sub(now()); remaining < pause {
			pause = remaining
		}
		sleep(pause)
	}
}

// exitPollInterval is the longest waitForExit waits between checks; the sleep
// before the deadline is shortened to what is left of it.
const exitPollInterval = 50 * time.Millisecond

// exitPollClock is the fake clock waitForExit's boundary tests run on: time
// stands still until the poll sleeps, and every sleep is recorded.
type exitPollClock struct {
	start  time.Time
	now    time.Time
	sleeps []time.Duration
}

func newExitPollClock() *exitPollClock {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &exitPollClock{start: start, now: start}
}

func (c *exitPollClock) Now() time.Time { return c.now }

func (c *exitPollClock) Sleep(d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
}

func (c *exitPollClock) elapsed() time.Duration { return c.now.Sub(c.start) }

// TestWaitForExitReturnsOnceGone drives the poll's success path: a process that
// goes away is reported gone as soon as the check says so, with the fake clock
// advancing only by the sleeps in between.
func TestWaitForExitReturnsOnceGone(t *testing.T) {
	t.Parallel()
	clk := newExitPollClock()
	polls := 0
	exited := func() bool { polls++; return polls == 3 }
	if !waitForExit(exited, 5*time.Second, clk.Now, clk.Sleep) {
		t.Fatal("a process that had gone by the third poll was not reported gone")
	}
	if polls != 3 {
		t.Fatalf("the poll asked %d times; it must stop at the first answer that the process is gone", polls)
	}
	want := []time.Duration{exitPollInterval, exitPollInterval}
	if !slices.Equal(clk.sleeps, want) {
		t.Fatalf("the poll slept %v between checks, want %v on the injected clock", clk.sleeps, want)
	}
}

// TestWaitForExitStopsAtTheDeadline drives the poll's refusal path: a process
// that never goes away is reported not gone at the deadline, and the last sleep
// is shortened to what the deadline leaves instead of a fixed step that would
// carry the poll past it.
func TestWaitForExitStopsAtTheDeadline(t *testing.T) {
	t.Parallel()
	const timeout = 120 * time.Millisecond
	clk := newExitPollClock()
	polls := 0
	exited := func() bool {
		polls++
		if polls > 100 {
			// A poll that ignores the deadline or sleeps outside the seam never
			// moves the fake clock past it, so without this bound the test
			// would hang instead of failing.
			t.Fatalf("the poll is still asking after %d checks with the fake clock short of the deadline: it honours neither the deadline nor the injected sleep", polls)
		}
		return false
	}
	if waitForExit(exited, timeout, clk.Now, clk.Sleep) {
		t.Fatal("a process that never went away was reported gone")
	}
	want := []time.Duration{exitPollInterval, exitPollInterval, timeout - 2*exitPollInterval}
	if !slices.Equal(clk.sleeps, want) {
		t.Fatalf("the poll slept %v, want %v: the last sleep must be clamped to the deadline", clk.sleeps, want)
	}
	if polls != 4 {
		t.Fatalf("the poll asked %d times, want 4 (once at the start and once after each sleep, the last of them at the deadline)", polls)
	}
	if elapsed := clk.elapsed(); elapsed != timeout {
		t.Fatalf("the poll ran %s of fake time, want it to end at the %s deadline", elapsed, timeout)
	}
}
