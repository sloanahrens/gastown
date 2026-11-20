package daemon

import (
	"context"
	"sync"
)

// nodePruneFlight keeps the node image prune to one run at a time. Its zero
// value is ready to use.
type nodePruneFlight struct {
	mu      sync.Mutex
	running bool
}

// Start launches run in its own goroutine and returns true, or, when a run is
// already going, starts nothing and returns false at once. The caller passes
// the daemon's long-lived context: the run derives from it, so it outlives the
// call and only canceling that context ends it. A panicking run is recovered
// and its mark cleared, so a bad prune cannot take the daemon down.
func (f *nodePruneFlight) Start(parent context.Context, run func(ctx context.Context)) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return false
	}
	f.running = true
	ctx, cancel := context.WithCancel(parent)
	go func() {
		defer cancel()
		defer func() { f.mu.Lock(); f.running = false; f.mu.Unlock() }()
		defer func() { _ = recover() }() // a panicking prune must not take the daemon down
		run(ctx)
	}()
	return true
}
