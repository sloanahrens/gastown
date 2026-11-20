package daemon

import (
	"context"
	"runtime"
	"testing"
)

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
	<-started
	if f.Start(context.Background(), func(context.Context) {}) {
		t.Fatal("Start while a run is going = true, want false")
	}
	select {
	case <-finished:
		t.Fatal("Start returned only once the run finished")
	default:
	}
	close(release)
	<-finished
	assertNodePruneFlightStarts(t, &f, "after the run finished")
}

// TestNodePruneFlightSurvivesAPanicAndCancels: a panicking run is recovered and
// clears its mark, and cancelling the parent cancels the run's context.
func TestNodePruneFlightSurvivesAPanicAndCancels(t *testing.T) {
	t.Parallel()

	var f nodePruneFlight
	panicked := make(chan struct{})
	if !f.Start(context.Background(), func(context.Context) {
		defer close(panicked)
		panic("prune exploded")
	}) {
		t.Fatal("Start = false, want true")
	}
	<-panicked
	assertNodePruneFlightStarts(t, &f, "after the run panicked")

	parent, cancel := context.WithCancel(context.Background())
	runCtx := make(chan context.Context, 1)
	done := make(chan struct{})
	if !f.Start(parent, func(ctx context.Context) {
		runCtx <- ctx
		<-ctx.Done()
		close(done)
	}) {
		t.Fatal("Start before cancelling the parent = false, want true")
	}
	ctx := <-runCtx
	cancel()
	if ctx.Err() == nil {
		t.Fatal("cancelling the parent left the run's context live")
	}
	<-done
}

// assertNodePruneFlightStarts waits, bounded, for a start to be accepted: the
// flight clears its mark on its own goroutine, just after run returns.
func assertNodePruneFlightStarts(t *testing.T, f *nodePruneFlight, when string) {
	t.Helper()
	done := make(chan struct{})
	for i := 0; i < 10000; i++ {
		if f.Start(context.Background(), func(context.Context) { close(done) }) {
			<-done
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("Start %s = false, want true: the mark never cleared", when)
}
