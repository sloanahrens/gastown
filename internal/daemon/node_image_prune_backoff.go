package daemon

import (
	"sync"
	"time"
)

const (
	nodePruneBackoffWindow = 30 * time.Minute
	nodePruneBackoffMax    = 3
)

// nodePruneBackoff decides when a failed node image prune may be retried: at
// least nodePruneBackoffWindow after its last failure, and never after
// nodePruneBackoffMax failures. Its zero value is ready to use (gt-u3d0tm).
type nodePruneBackoff struct {
	mu      sync.Mutex
	entries map[string]nodePruneAttempt
}

type nodePruneAttempt struct {
	failures int
	last     time.Time
}

// Failed records a failure for runID at now; the empty id is ignored.
func (b *nodePruneBackoff) Failed(runID string, now time.Time) {
	if runID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = make(map[string]nodePruneAttempt)
	}
	a := b.entries[runID]
	a.failures++
	a.last = now
	b.entries[runID] = a
}

// Allowed reports whether runID may be pruned at now.
func (b *nodePruneBackoff) Allowed(runID string, now time.Time) bool {
	if runID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a, failed := b.entries[runID]
	return !failed || (a.failures < nodePruneBackoffMax && !now.Before(a.last.Add(nodePruneBackoffWindow)))
}
