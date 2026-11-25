package daemon

import (
	"sync"
	"time"
)

const (
	nodePruneBackoffWindow = 30 * time.Minute
	nodePruneBackoffMax    = 3
	// nodePruneBackoffEntries bounds the map so a long-lived daemon does not
	// keep every run id it ever failed. Entries are not dropped when their
	// window passes: the count must survive a retry, or a run that fails
	// every time would never reach the limit. A full map instead drops the
	// id whose last failure is oldest, which lets that id be tried again,
	// the harmless direction a process restart already takes (gt-42832l).
	nodePruneBackoffEntries = 512
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
	// Only a new id grows the map, so only a new id makes room.
	if _, ok := b.entries[runID]; !ok && len(b.entries) >= nodePruneBackoffEntries {
		b.forgetOldest()
	}
	a := b.entries[runID]
	a.failures++
	a.last = now
	b.entries[runID] = a
}

// forgetOldest drops the entry whose last failure is oldest, making room in a
// full map. It must be called with b.mu held.
func (b *nodePruneBackoff) forgetOldest() {
	var oldest string
	var oldestAt time.Time
	first := true
	for id, a := range b.entries {
		if first || a.last.Before(oldestAt) {
			oldest, oldestAt, first = id, a.last, false
		}
	}
	delete(b.entries, oldest)
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
