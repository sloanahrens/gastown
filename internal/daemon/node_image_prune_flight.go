package daemon

import (
	"context"
	"fmt"
	"sync"
)

// nodePrunePanicMax is the most of a recovered panic's text report gets.
const nodePrunePanicMax = 300

// nodePruneFlight keeps the node image prune to one run at a time. Its zero
// value is ready to use.
type nodePruneFlight struct {
	mu      sync.Mutex
	running bool
	// finished closes when the running run ends, so a waiter can tell the
	// flight is free without polling.
	finished chan struct{}
}

// Start launches run in its own goroutine and returns true, or starts nothing
// and returns false at once: when a run is already going, and when parent is
// already done, since that run's context would be canceled on arrival. The
// caller passes the daemon's long-lived context: the run derives from it, so
// it outlives the call and only canceling that context ends it.
//
// A panicking run is recovered so the daemon survives and counts as a failed
// run: the mark is cleared so the next heartbeat may start another, and report
// is called once with the panic value as text, cut to nodePrunePanicMax bytes.
// report runs on the run's goroutine and must not block, panic or be nil.
func (f *nodePruneFlight) Start(parent context.Context, run func(ctx context.Context), report func(panicValue any)) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running || parent.Err() != nil {
		return false
	}
	f.running = true
	f.finished = make(chan struct{})
	finished := f.finished
	ctx, cancel := context.WithCancel(parent)
	go func() {
		defer cancel()
		defer close(finished)
		defer func() {
			if r := recover(); r != nil {
				text := fmt.Sprint(r)
				if len(text) > nodePrunePanicMax {
					text = text[:nodePrunePanicMax]
				}
				report(text)
			}
		}()
		defer func() {
			f.mu.Lock()
			f.running = false
			f.mu.Unlock()
		}()
		run(ctx)
	}()
	return true
}

// done returns a channel closed when the current run ends.
func (f *nodePruneFlight) done() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished
}
