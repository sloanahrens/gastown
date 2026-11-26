package landworker

import (
	"context"
	"reflect"
	"testing"
)

// hold waits for release, or returns at the test's bound.
func hold(ctx context.Context, release <-chan struct{}) {
	select {
	case <-release:
	case <-ctx.Done():
	}
}

// TestBookQueueRunsInOrder: jobs are serial, Add returns while one runs, a panic is survived.
func TestBookQueueRunsInOrder(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	var q BookQueue

	var order []string
	firstEntered, firstRelease := make(chan struct{}), make(chan struct{})
	secondEntered, afterPanic := make(chan struct{}), make(chan struct{})

	q.Add("gt-first", func() {
		order = append(order, "first start")
		close(firstEntered)
		hold(ctx, firstRelease)
		order = append(order, "first return")
	})
	recvBounded(t, ctx, firstEntered, "the first job starting")

	// Add on its own goroutine: one that waits for the blocked job fails by name.
	added := make(chan struct{})
	go func() {
		q.Add("gt-second", func() {
			order = append(order, "second start")
			close(secondEntered)
		})
		close(added)
	}()
	recvBounded(t, ctx, added, "Add returning while the first job is blocked")
	select {
	case <-secondEntered:
		t.Fatal("the second job started before the first returned")
	default:
	}

	close(firstRelease)
	recvBounded(t, ctx, secondEntered, "the second job starting after the first returned")
	q.Add("gt-panic", func() { panic("bookkeeping blew up") })
	q.Add("gt-last", func() { close(afterPanic) })
	recvBounded(t, ctx, afterPanic, "the job after the panicking one starting")

	if want := []string{"first start", "first return", "second start"}; !reflect.DeepEqual(order, want) {
		t.Errorf("jobs ran in order %v, want %v", order, want)
	}
}

// TestBookQueueHolds: Holds spans a bead's job, from Add until that job returns.
func TestBookQueueHolds(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	var q BookQueue

	firstEntered, firstRelease := make(chan struct{}), make(chan struct{})
	secondEntered, secondRelease := make(chan struct{}), make(chan struct{})
	thirdEntered := make(chan struct{})

	q.Add("gt-first", func() { close(firstEntered); hold(ctx, firstRelease) })
	recvBounded(t, ctx, firstEntered, "the first job starting")
	q.Add("gt-second", func() { close(secondEntered); hold(ctx, secondRelease) })
	if !q.Holds("gt-first") || !q.Holds("gt-second") {
		t.Errorf("Holds with the first running and the second queued = %v, %v, want true, true",
			q.Holds("gt-first"), q.Holds("gt-second"))
	}

	close(firstRelease)
	recvBounded(t, ctx, secondEntered, "the second job starting after the first returned")
	if q.Holds("gt-first") {
		t.Error("Holds(gt-first) = true after its job returned, want false")
	}
	if !q.Holds("gt-second") {
		t.Error("Holds(gt-second) = false while its job runs, want true")
	}

	// A third job starting proves the second returned: its hold is released first.
	q.Add("gt-third", func() { close(thirdEntered) })
	close(secondRelease)
	recvBounded(t, ctx, thirdEntered, "the third job starting after the second returned")
	if q.Holds("gt-second") {
		t.Error("Holds(gt-second) = true after its job returned, want false")
	}
}
