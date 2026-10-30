package landworker

import (
	"sync"
	"time"
)

// LandingQueueMaxAge bounds how old a published ready-to-land count may be and
// still answer for the queue now: one landing pass (the worker's default
// interval) plus the pass's own duration. A count older than this is what an
// earlier pass saw, so a reader that finds one reads the board itself.
const LandingQueueMaxAge = 3 * time.Minute

// LandingQueueView holds the ready-to-land count each rig's worker published
// at the top of its last pass, so a metrics sample can report the queue
// without listing the same board in every rig (gt-hw6ih).
//
// The workers publish from their own goroutines while samples read, so every
// access holds the view's mutex. A nil view is usable: it keeps no count and
// answers none, which leaves a reader with its own board read.
type LandingQueueView struct {
	now func() time.Time

	mu     sync.Mutex
	counts map[string]landingQueueCount
}

// landingQueueCount is one rig's published count and the view clock's time
// when it was published.
type landingQueueCount struct {
	count int
	at    time.Time
}

// NewLandingQueueView returns an empty view that stamps each publish with now;
// a nil now is the wall clock.
func NewLandingQueueView(now func() time.Time) *LandingQueueView {
	if now == nil {
		now = time.Now
	}
	return &LandingQueueView{now: now, counts: make(map[string]landingQueueCount)}
}

// Publish records rig's count, replacing what an earlier pass published for
// it.
func (v *LandingQueueView) Publish(rig string, count int) {
	if v == nil {
		return
	}
	at := v.now()
	v.mu.Lock()
	v.counts[rig] = landingQueueCount{count: count, at: at}
	v.mu.Unlock()
}

// Counts returns the counts still fresh at now, keyed by rig. A count older
// than LandingQueueMaxAge, and a rig no worker has published, are left out so
// the reader reads that rig's board: a stale count is not an empty queue, and
// neither is one that was never taken. A count stamped after now is kept,
// because two clocks disagreeing by less than the bound is not staleness.
func (v *LandingQueueView) Counts(now time.Time) map[string]int {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var out map[string]int
	for rig, c := range v.counts {
		if now.Sub(c.at) > LandingQueueMaxAge {
			continue
		}
		if out == nil {
			out = make(map[string]int, len(v.counts))
		}
		out[rig] = c.count
	}
	return out
}
