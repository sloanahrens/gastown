package cmd

import (
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// doltWriteDeltas turns the gateway's cumulative writes-per-actor counters into
// the writes between two reads (row (3) of the operator's 2026-11-18 list, gt-e14l54).
// The panel reads on several goroutines, so a read holds the lock.
type doltWriteDeltas struct {
	mu   sync.Mutex
	seen bool
	prev map[string]uint64
}

// add counts what each actor wrote since the previous read into buckets under
// its name at at. The first read adds nothing, since the counters began before
// the dashboard did; a counter that went down (the gateway restarted) adds the
// new count; an actor first seen on a later read adds its count.
func (w *doltWriteDeltas) add(buckets *dashboard.Buckets, at time.Time, writes map[string]uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen {
		for actor, now := range writes {
			delta := now
			if before := w.prev[actor]; now >= before {
				delta = now - before
			}
			buckets.Add(at, actor, int(delta))
		}
	}
	w.seen, w.prev = true, writes
}
