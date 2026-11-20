package bdgateway

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
)

// TestTakeWindowStatsReturnsTheWindowPeakThenResets: the first read answers the
// highest in-flight count since start, with the slots held now, the capacity and
// the refusals; it resets the window to the calls still in flight, so the next
// read answers that count rather than the old peak.
func TestTakeWindowStatsReturnsTheWindowPeakThenResets(t *testing.T) {
	t.Parallel()
	const capacity = 2
	runner := &capRunner{bound: waitCtx(t), started: make(chan struct{}, capacity)}
	e, err := New(Options{Binary: "/opt/bd/bd", WorkspaceRoot: testWorkspace(t), Home: "/home/bdgw",
		Path: "/usr/bin:/bin", DoltHost: "gt-dolt", DoltPort: "3307", MaxConcurrent: capacity, runner: runner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, capacity)
	for i := 0; i < capacity; i++ {
		go func() {
			done <- e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, io.Discard)
		}()
		select {
		case <-runner.started:
		case <-waitCtx(t).Done():
			t.Fatal("a call was not started within the test's bound")
		}
	}
	// Both slots are held, so the cap turns this third call away.
	if err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, io.Discard); !errors.Is(err, ErrTooManyCalls) {
		t.Fatalf("the call over the cap = %v, want ErrTooManyCalls", err)
	}
	// End the second call and wait for it, so its slot is released before the read.
	runner.mu.Lock()
	procs := slices.Clone(runner.procs)
	runner.mu.Unlock()
	procs[1].finish(0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the second call: %v", err)
		}
	case <-waitCtx(t).Done():
		t.Fatal("the second call did not return within the test's bound")
	}
	cases := []struct {
		read string
		want [4]uint64 // peak, in flight, capacity, refused
	}{
		{"first", [4]uint64{2, 1, capacity, 1}},
		{"after the reset", [4]uint64{1, 1, capacity, 1}},
	}
	for _, tc := range cases {
		peak, inFlight, slots, refused := e.TakeWindowStats()
		if got := [4]uint64{peak, inFlight, slots, refused}; got != tc.want {
			t.Errorf("%s read = (peak %d, in flight %d, capacity %d, refused %d), want %v",
				tc.read, peak, inFlight, slots, refused, tc.want)
		}
	}
	procs[0].finish(0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the first call: %v", err)
		}
	case <-waitCtx(t).Done():
		t.Fatal("the first call did not return within the test's bound")
	}
}

// TestTakeWindowStatsZeroExecutor: an executor with no slots answers zeros
// rather than panicking, since a typed nil can still reach a caller.
func TestTakeWindowStatsZeroExecutor(t *testing.T) {
	t.Parallel()
	var typedNil *Executor
	for name, e := range map[string]*Executor{"typed nil": typedNil, "zero value": {}} {
		if peak, inFlight, slots, refused := e.TakeWindowStats(); peak != 0 || inFlight != 0 || slots != 0 || refused != 0 {
			t.Errorf("%s: TakeWindowStats = (%d, %d, %d, %d), want zeros", name, peak, inFlight, slots, refused)
		}
	}
}
