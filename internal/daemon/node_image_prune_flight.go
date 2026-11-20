package daemon

import (
	"context"
	"log"
	"sync"
)

// nodePruneFlight keeps the node image prune to one run at a time. Its zero
// value is ready to use.
type nodePruneFlight struct {
	mu      sync.Mutex
	running bool
	// finished closes when the running run ends, so a waiter can tell the
	// flight is free without polling.
	finished chan struct{}
	// logf records a run that panicked; nil logs through the standard logger.
	// It is read on the run's goroutine, so set it before the first Start.
	logf func(format string, args ...any)
}

// Start launches run in its own goroutine and returns true, or starts nothing
// and returns false at once: when a run is already going, and when parent is
// already done, since that run's context would be canceled on arrival. The
// caller passes the daemon's long-lived context: the run derives from it, so
// it outlives the call and only canceling that context ends it. A panicking
// run is recovered, logged, and its mark cleared, so a bad prune cannot take
// the daemon down.
func (f *nodePruneFlight) Start(parent context.Context, run func(ctx context.Context)) bool {
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
				f.report("node image prune: run panicked: %v", r)
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

// report logs a recovered panic through logf, or the standard logger when no
// seam was set.
func (f *nodePruneFlight) report(format string, args ...any) {
	logf := f.logf
	if logf == nil {
		logf = log.Printf
	}
	logf(format, args...)
}
