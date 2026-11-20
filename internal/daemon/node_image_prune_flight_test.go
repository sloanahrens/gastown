package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// nodePruneFlightWait bounds every wait in these tests: a signal that never
// arrives fails the test naming what it waited for instead of hanging the
// package until the go test timeout.
const nodePruneFlightWait = 30 * time.Second

// awaitNodePruneFlight receives from ch, or fails the test naming what it
// waited for once nodePruneFlightWait passes.
func awaitNodePruneFlight[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), nodePruneFlightWait)
	defer cancel()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		var zero T
		t.Fatalf("waited %v for %s", nodePruneFlightWait, what)
		return zero
	}
}

// TestNodePruneFlightStartsOneAtATime: a second Start while a run holds the
// flight is refused, Start returns before that run finishes, and a Start after
// it finishes is accepted again.
func TestNodePruneFlightStartsOneAtATime(t *testing.T) {
	t.Parallel()

	var f nodePruneFlight
	started, release := make(chan struct{}), make(chan struct{})
	// The first Start runs in a goroutine and reports its answer: a Start that
	// ran the work inline would block on release, so awaiting the answer fails
	// (bounded, naming it) instead of hanging the package.
	firstStart := make(chan bool, 1)
	go func() {
		firstStart <- f.Start(context.Background(), func(context.Context) {
			close(started)
			// The run waits on release or the test's end, never forever.
			select {
			case <-release:
			case <-t.Context().Done():
			}
		}, nil)
	}()
	if !awaitNodePruneFlight(t, firstStart, "the first Start to return while its run is still blocked") {
		t.Fatal("first Start = false, want true")
	}
	awaitNodePruneFlight(t, started, "the first run to start")
	secondRan := make(chan struct{}, 1)
	if f.Start(context.Background(), func(context.Context) { secondRan <- struct{}{} }, nil) {
		t.Fatal("Start while a run is going = true, want false")
	}
	select {
	case <-secondRan:
		t.Fatal("the refused Start launched its run")
	default:
	}
	close(release)
	awaitNodePruneFlight(t, f.done(), "the first run to end")
	if !f.Start(context.Background(), func(context.Context) {}, nil) {
		t.Fatal("Start after the run finished = false, want true: the mark never cleared")
	}
	awaitNodePruneFlight(t, f.done(), "the second run to end")
}

// TestNodePruneFlightSurvivesAPanic: a panicking run is recovered so the
// daemon survives, report is called once with the panic value cut to the cap,
// and the mark is cleared so a later Start is accepted.
func TestNodePruneFlightSurvivesAPanic(t *testing.T) {
	t.Parallel()

	value := strings.Repeat("x", nodePrunePanicMax+100)
	var f nodePruneFlight
	reported := make(chan any, 1)
	if !f.Start(context.Background(), func(context.Context) { panic(value) },
		func(v any) { reported <- v }) {
		t.Fatal("Start = false, want true")
	}
	awaitNodePruneFlight(t, f.done(), "the panicking run to end")
	if got := awaitNodePruneFlight(t, reported, "the panic report"); got != value[:nodePrunePanicMax] {
		t.Fatalf("report got a value of %d bytes, want the value cut to %d", len(fmt.Sprint(got)), nodePrunePanicMax)
	}
	select {
	case again := <-reported:
		t.Fatalf("report called again with %q, want once", again)
	default:
	}
	if !f.Start(context.Background(), func(context.Context) {}, nil) {
		t.Fatal("Start after the panic = false, want true: the mark never cleared")
	}
	awaitNodePruneFlight(t, f.done(), "the run after the panic to end")
}

// TestNodePruneFlightRunContextCancelsWithTheParent: the run's context derives
// from the parent, not from the caller.
func TestNodePruneFlightRunContextCancelsWithTheParent(t *testing.T) {
	t.Parallel()

	var f nodePruneFlight
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard, stop := context.WithTimeout(t.Context(), nodePruneFlightWait)
	defer stop()
	runCtx := make(chan context.Context, 1)
	if !f.Start(parent, func(ctx context.Context) {
		runCtx <- ctx
		select {
		case <-ctx.Done():
		case <-guard.Done(): // the test ended without the parent's cancel reaching the run
		}
	}, nil) {
		t.Fatal("Start = false, want true")
	}
	ctx := awaitNodePruneFlight(t, runCtx, "the run to start")
	cancel()
	if ctx.Err() == nil {
		t.Fatal("canceling the parent left the run's context live")
	}
	awaitNodePruneFlight(t, f.done(), "the run to end")
}

// TestNodePruneFlightDoneParent: a parent already done starts no run, and
// leaves the flight free for the next Start.
func TestNodePruneFlightDoneParent(t *testing.T) {
	t.Parallel()

	var f nodePruneFlight
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ran := make(chan struct{}, 1)
	if f.Start(parent, func(context.Context) { ran <- struct{}{} }, nil) {
		t.Fatal("Start with a done parent = true, want false")
	}
	select {
	case <-ran:
		t.Fatal("Start with a done parent launched its run")
	default:
	}
	if !f.Start(context.Background(), func(context.Context) {}, nil) {
		t.Fatal("Start after the refusal = false, want true: the mark was left set")
	}
	awaitNodePruneFlight(t, f.done(), "the run after the refusal to end")
}
