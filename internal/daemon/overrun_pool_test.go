package daemon

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
)

// The pool never has more than its limit of reads in flight and runs each job once
// (gt-2w32ve). Fake reads block on a channel the test releases: no sleep, no timer.
func TestOverrunPassBoundedReads(t *testing.T) {
	t.Parallel()
	const jobs = 12
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	ran := make([]int, jobs)
	started := make(chan int, jobs)
	release := make(chan struct{})
	var releaseOnce sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		overrunReadPool(jobs, overrunReadPoolSize, func(i int) {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			ran[i]++
			mu.Unlock()
			started <- i
			<-release
			mu.Lock()
			inFlight--
			mu.Unlock()
		})
	}()
	// The first wave fills the pool; yield so a pool with no bound has the chance to start
	// the rest before anything is released, then release.
	ctx, cancel := context.WithTimeout(context.Background(), workspaceHangGuard)
	defer cancel()
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	for range overrunReadPoolSize {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatalf("fewer than %d reads started within %s", overrunReadPoolSize, workspaceHangGuard)
		}
	}
	for range 1000 {
		runtime.Gosched()
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("the pool did not return within %s", workspaceHangGuard)
	}
	if maxInFlight > overrunReadPoolSize {
		t.Fatalf("%d reads in flight at once, want at most %d", maxInFlight, overrunReadPoolSize)
	}
	for i, n := range ran {
		if n != 1 {
			t.Errorf("job %d ran %d times, want once", i, n)
		}
	}
}

// A read that panics is re-raised in the caller once every read has returned, so the
// caller's recover sees it and no read is left running (gt-lxzghd).
func TestOverrunPassPanicReachesTheCaller(t *testing.T) {
	t.Parallel()
	const jobs = 8
	var mu sync.Mutex
	ran := make([]int, jobs)
	var got any
	func() {
		defer func() { got = recover() }()
		overrunReadPool(jobs, overrunReadPoolSize, func(i int) {
			mu.Lock()
			ran[i]++
			mu.Unlock()
			if i == 3 {
				panic("boom")
			}
		})
	}()
	if got != "boom" {
		t.Fatalf("the caller recovered %v, want the panic value boom", got)
	}
	for i, n := range ran {
		if n != 1 {
			t.Errorf("job %d ran %d times, want once", i, n)
		}
	}
}

// The pool's edges (gt-fc1n18): a limit below one runs one read at a time, no reads call
// nothing, and a limit of one starts the reads in index order.
func TestOverrunPassEdges(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		var mu sync.Mutex
		ran := make([]int, 5)
		runPoolBounded(t, fmt.Sprintf("limit %d", limit), 5, limit, func(i int) {
			mu.Lock()
			ran[i]++
			mu.Unlock()
		})
		for i, n := range ran {
			if n != 1 {
				t.Errorf("limit %d: job %d ran %d times, want once", limit, i, n)
			}
		}
	}
	for _, n := range []int{0, -3} {
		runPoolBounded(t, fmt.Sprintf("n %d", n), n, overrunReadPoolSize, func(i int) {
			t.Errorf("n %d: read %d was called, want no read", n, i)
		})
	}
	var order []int
	runPoolBounded(t, "limit 1", 5, 1, func(i int) { order = append(order, i) })
	if !slices.Equal(order, []int{0, 1, 2, 3, 4}) {
		t.Fatalf("a limit of 1 started the reads in order %v, want 0 to 4", order)
	}
}

// runPoolBounded runs overrunReadPool in a goroutine and waits for it for the hang guard,
// so a pool that never returns or that panics fails the row with a message naming it,
// instead of ending the package (gt-fc1n18). Every row of the edge test goes through it.
func runPoolBounded(t *testing.T, row string, n, limit int, read func(i int)) {
	t.Helper()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		overrunReadPool(n, limit, read)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), workspaceHangGuard)
	defer cancel()
	select {
	case p := <-done:
		if p != nil {
			t.Fatalf("overrunReadPool (%s) panicked: %v", row, p)
		}
	case <-ctx.Done():
		t.Fatalf("overrunReadPool (%s) did not return within %s", row, workspaceHangGuard)
	}
}
