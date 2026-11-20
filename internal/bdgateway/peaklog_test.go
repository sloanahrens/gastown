package bdgateway

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// peakLogBound bounds every wait in these tests: the goroutine must take a tick
// or answer its context well inside it, so a missed tick fails here rather than
// hanging the suite.
const peakLogBound = 5 * time.Second

// TestRunPeakLogWritesOnlyBusyMinutes: a tick whose window peak is above zero
// writes one line naming the peak against the capacity, the calls in flight now
// and the refusals since start; a tick with a zero peak writes no line, and its
// window is still taken so the next busy tick reports only its own peak.
func TestRunPeakLogWritesOnlyBusyMinutes(t *testing.T) {
	t.Parallel()
	bound, cancelBound := context.WithTimeout(t.Context(), peakLogBound)
	defer cancelBound()
	runCtx, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()
	ticks, lines, done := make(chan time.Time), make(chan string, 4), make(chan struct{})
	// Each reading is peak, in flight, capacity and refused, one per tick.
	readings := [][4]uint64{{3, 1, 8, 2}, {0, 0, 8, 2}, {5, 2, 8, 3}}
	takes := 0
	take := func() (peak, inFlight, capacity, refused uint64) {
		r := readings[takes]
		takes++
		return r[0], r[1], r[2], r[3]
	}
	logf := func(format string, args ...any) { lines <- fmt.Sprintf(format, args...) }
	go func() {
		defer close(done)
		RunPeakLog(runCtx, ticks, take, logf)
	}()
	tick := func() {
		select {
		case ticks <- time.Time{}:
		case <-bound.Done():
			t.Fatalf("RunPeakLog did not take tick %d within the test's bound", takes)
		}
	}
	next := func() string {
		select {
		case line := <-lines:
			return line
		case <-bound.Done():
			t.Fatalf("RunPeakLog wrote no line for tick %d within the test's bound", takes)
		}
		return ""
	}

	tick()
	if got, want := next(), "slots: peak 3 of 8 in the last minute, 1 in flight now, 2 refused since start"; got != want {
		t.Errorf("the busy tick's line = %q, want %q", got, want)
	}
	// The idle tick writes nothing: the next line the test reads is the third
	// reading's, and no line can overtake it since the loop finishes a tick
	// before it takes the next one.
	tick()
	tick()
	if got, want := next(), "slots: peak 5 of 8 in the last minute, 2 in flight now, 3 refused since start"; got != want {
		t.Errorf("the line after the idle tick = %q, want the next busy tick's %q", got, want)
	}
	cancelRun()
	select {
	case <-done:
	case <-bound.Done():
		t.Fatal("RunPeakLog did not return after its context ended")
	}
	if takes != len(readings) {
		t.Errorf("take ran %d times for %d ticks, want once per tick", takes, len(readings))
	}
	select {
	case line := <-lines:
		t.Errorf("RunPeakLog wrote %q for a tick with a zero peak, want no line", line)
	default:
	}
}

// TestRunPeakLogStopsWithItsContext: with no tick ever sent, an ending context
// returns the goroutine rather than leaving it waiting on the ticks channel.
func TestRunPeakLogStopsWithItsContext(t *testing.T) {
	t.Parallel()
	bound, cancelBound := context.WithTimeout(t.Context(), peakLogBound)
	defer cancelBound()
	runCtx, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunPeakLog(runCtx, make(chan time.Time),
			func() (uint64, uint64, uint64, uint64) { return 0, 0, 0, 0 },
			func(string, ...any) {})
	}()
	cancelRun()
	select {
	case <-done:
	case <-bound.Done():
		t.Fatal("RunPeakLog did not return after its context ended")
	}
}
