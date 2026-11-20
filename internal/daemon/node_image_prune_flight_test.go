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
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	if !f.Start(context.Background(), func(context.Context) {
		close(started)
		<-release
		close(finished)
	}) {
		t.Fatal("first Start = false, want true")
	}
	awaitNodePruneFlight(t, started, "the first run to start")
	secondRan := make(chan struct{}, 1)
	if f.Start(context.Background(), func(context.Context) { secondRan <- struct{}{} }) {
		t.Fatal("Start while a run is going = true, want false")
	}
	select {
	case <-secondRan:
		t.Fatal("the refused Start launched its run")
	default:
	}
	select {
	case <-finished:
		t.Fatal("Start returned only once the run finished")
	default:
	}
	close(release)
	awaitNodePruneFlight(t, f.done(), "the first run to end")
	if !f.Start(context.Background(), func(context.Context) {}) {
		t.Fatal("Start after the run finished = false, want true: the mark never cleared")
	}
	awaitNodePruneFlight(t, f.done(), "the second run to end")
}

// TestNodePruneFlightSurvivesAPanic: a panicking run is recovered, logged, and
// clears its mark so the daemon stays up and a later Start is accepted.
func TestNodePruneFlightSurvivesAPanic(t *testing.T) {
	t.Parallel()

	var f nodePruneFlight
	logged := make(chan string, 1)
	f.logf = func(format string, args ...any) { logged <- fmt.Sprintf(format, args...) }
	if !f.Start(context.Background(), func(context.Context) {
		panic("prune exploded")
	}) {
		t.Fatal("Start = false, want true")
	}
	awaitNodePruneFlight(t, f.done(), "the panicking run to end")
	select {
	case line := <-logged:
		if !strings.Contains(line, "panicked") || !strings.Contains(line, "prune exploded") {
			t.Fatalf("recovered panic logged as %q, want the panic named", line)
		}
	default:
		t.Fatal("recovered panic was not logged")
	}
	if !f.Start(context.Background(), func(context.Context) {}) {
		t.Fatal("Start after the run panicked = false, want true: the mark never cleared")
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
	}) {
		t.Fatal("Start = false, want true")
	}
	ctx := awaitNodePruneFlight(t, runCtx, "the run to start")
	cancel()
	if ctx.Err() == nil {
		t.Fatal("canceling the parent left the run's context live")
	}
	awaitNodePruneFlight(t, f.done(), "the run to end")
}

// TestNodePruneFlightRefusesADoneParent: a parent already canceled starts no
// run, and leaves the flight free for the next Start.
func TestNodePruneFlightRefusesADoneParent(t *testing.T) {
	t.Parallel()

	var f nodePruneFlight
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ran := make(chan struct{}, 1)
	if f.Start(parent, func(context.Context) { ran <- struct{}{} }) {
		t.Fatal("Start with a canceled parent = true, want false")
	}
	select {
	case <-ran:
		t.Fatal("Start with a canceled parent launched its run")
	default:
	}
	if !f.Start(context.Background(), func(context.Context) {}) {
		t.Fatal("Start after the refusal = false, want true: the mark was left set")
	}
	awaitNodePruneFlight(t, f.done(), "the run after the refusal to end")
}
