package daemon

import (
	"runtime"
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
	for range overrunReadPoolSize {
		<-started
	}
	for range 1000 {
		runtime.Gosched()
	}
	close(release)
	<-done
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
