package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// The duplicate check compares a pool bead on its design and notes text, which
// bd list never carries, so the pool fetch used to run one batched bd show over
// every pool bead on every tick: 180 beads a minute, a read that overran when
// the host was busy and was killed with the tick (gt-l0vlf, after gt-u8rbh cut
// the repeats inside one tick but not the repeats across ticks).
//
// duplicateDetailCache keeps what that show returned, keyed by bead id and the
// updated_at of the bd list row the tick already has. A bead whose row still
// carries the cached updated_at has the same design and notes, so a tick shows
// only the beads that changed, and no more than duplicateDetailMissLimit of
// them: the rest are compared on list text, as a failed show leaves them, with
// the degraded warning naming the count, and the next ticks fill them in.

// duplicateDetailMissLimit is the most beads one tick shows for design and
// notes. A warm cache needs a handful of reads a tick; the limit is what a
// cold one, or a burst of edits, may spend before the rest wait for the next
// tick.
const duplicateDetailMissLimit = 40

// specDetailCache is the cache the tick's pool fetch reads through, nil when
// the tick keeps none (a tick run by hand). runSpecDispatchAt sets it.
var specDetailCache atomic.Pointer[duplicateDetailCache]

// cachedDetail is what one show recovered for a bead, valid while the bead's
// list row still says UpdatedAt.
type cachedDetail struct {
	UpdatedAt string      `json:"updated_at"`
	Design    string      `json:"design,omitempty"`
	Notes     string      `json:"notes,omitempty"`
	Deps      []cachedDep `json:"deps,omitempty"`
}

// cachedDep is a dependency edge as the show gave it. Status and CloseReason
// are the target's at the time; a read refreshes them from the pool, because a
// blocker closing does not touch the dependent's updated_at.
type cachedDep struct {
	ID          string `json:"id"`
	Type        string `json:"type,omitempty"`
	Status      string `json:"status,omitempty"`
	CloseReason string `json:"close_reason,omitempty"`
}

// duplicateDetailFile is the cache's file.
type duplicateDetailFile struct {
	Owner   string                  `json:"owner"`
	Entries map[string]cachedDetail `json:"entries"`
}

// duplicateDetailCache is the daemon's design/notes cache for the duplicate
// check's pool. One value serves one tick: its counters are that tick's.
type duplicateDetailCache struct {
	limit int
	path  string // "" keeps the cache in memory
	owner string

	mu      sync.Mutex
	entries map[string]cachedDetail
	dirty   bool
	// The beads this tick showed, and the ones it answered from the cache.
	fetched map[string]bool
	cached  map[string]bool
	unread  map[string]bool
}

// newDuplicateDetailCache is an empty cache holding at most limit shows a tick,
// kept in path when path is not empty. owner names the process the file
// belongs to.
func newDuplicateDetailCache(path, owner string, limit int) *duplicateDetailCache {
	return &duplicateDetailCache{
		limit: limit, path: path, owner: owner,
		entries: map[string]cachedDetail{},
		fetched: map[string]bool{}, cached: map[string]bool{}, unread: map[string]bool{},
	}
}

// loadDuplicateDetailCache is newDuplicateDetailCache holding what path kept
// for owner. A file that is missing, unreadable, or written under another
// owner holds nothing: the cache is the daemon's, dropped when it restarts.
func loadDuplicateDetailCache(path, owner string, limit int) *duplicateDetailCache {
	c := newDuplicateDetailCache(path, owner, limit)
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var f duplicateDetailFile
	if json.Unmarshal(data, &f) != nil || f.Owner != owner || f.Entries == nil {
		return c
	}
	c.entries = f.Entries
	return c
}

// specDetailCacheFor is the cache a tick of the daemon named by owner reads
// through, nil for a tick no daemon started.
func specDetailCacheFor(townRoot, owner string) *duplicateDetailCache {
	if owner == "" {
		return nil
	}
	return loadDuplicateDetailCache(dispatch.SpecDetailCachePath(townRoot), owner, duplicateDetailMissLimit)
}

// Counts is how many beads this tick showed, answered from the cache, and left
// unread for the limit.
func (c *duplicateDetailCache) Counts() (fetched, cached, unread int) {
	if c == nil {
		return 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.fetched), len(c.cached), len(c.unread)
}

// read recovers the detail of every row. A row the cache holds at the row's
// updated_at is answered from it; the others are shown, up to the limit this
// tick has left, and the rest come back without a detail, counted in deferred.
// known is every row of the pool being built: it is where a cached edge's
// target status is refreshed from. An error is the show's, and the details it
// returns are the ones the cache answered.
func (c *duplicateDetailCache) read(store beads.Client, rows []*beads.Issue, known []*beads.Issue) (details map[string]duplicateDetail, deferred int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	status := make(map[string]*beads.Issue, len(known))
	for _, row := range known {
		status[row.ID] = row
	}
	details = make(map[string]duplicateDetail, len(rows))
	var missing []*beads.Issue
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		entry, ok := c.entries[row.ID]
		if !ok || row.UpdatedAt == "" || entry.UpdatedAt != row.UpdatedAt {
			missing = append(missing, row)
			continue
		}
		if c.refresh(&entry, status) {
			c.entries[row.ID] = entry
			c.dirty = true
		}
		details[row.ID] = entry.detail()
		if !c.fetched[row.ID] {
			c.cached[row.ID] = true
		}
	}

	room := c.limit - len(c.fetched)
	if room < 0 {
		room = 0
	}
	if c.limit <= 0 {
		room = len(missing)
	}
	if len(missing) > room {
		for _, row := range missing[room:] {
			c.unread[row.ID] = true
		}
		deferred = len(missing) - room
		missing = missing[:room]
	}
	if len(missing) > 0 {
		ids := make([]string, len(missing))
		for i, row := range missing {
			ids[i] = row.ID
			c.fetched[row.ID] = true
			delete(c.cached, row.ID)
			delete(c.unread, row.ID)
		}
		shown, showErr := store.ShowMultiple(ids)
		if showErr != nil {
			err = fmt.Errorf("fetching design/notes for %d bead(s): %w", len(ids), showErr)
		}
		for _, row := range missing {
			is := shown[row.ID]
			if is == nil {
				continue
			}
			detail := duplicateDetail{
				Design:       is.Design,
				Notes:        is.Notes,
				Blockers:     blockingDepIDs(is.Dependencies),
				OpenBlockers: beads.UnresolvedBlockerIDs(is.Dependencies),
			}
			details[row.ID] = detail
			if row.UpdatedAt == "" {
				continue
			}
			c.entries[row.ID] = cachedDetail{UpdatedAt: row.UpdatedAt, Design: is.Design, Notes: is.Notes, Deps: cachedDepsOf(is.Dependencies)}
			c.dirty = true
		}
	}
	c.save()
	return details, deferred, err
}

// refresh sets each cached edge's status to its target's as the pool lists it
// and reports whether any changed. An edge to a bead the pool does not list (a
// bead of another rig, or one closed before the lookback) keeps the status it
// was read with.
func (c *duplicateDetailCache) refresh(entry *cachedDetail, status map[string]*beads.Issue) bool {
	changed := false
	for i, dep := range entry.Deps {
		row, ok := status[beads.ExtractIssueID(dep.ID)]
		if !ok || (row.Status == dep.Status && row.CloseReason == dep.CloseReason) {
			continue
		}
		if !changed {
			entry.Deps = append([]cachedDep(nil), entry.Deps...)
			changed = true
		}
		entry.Deps[i].Status, entry.Deps[i].CloseReason = row.Status, row.CloseReason
	}
	return changed
}

// retain drops the entries of beads the pool no longer lists, and saves.
func (c *duplicateDetailCache) retain(ids map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.entries {
		if !ids[id] {
			delete(c.entries, id)
			c.dirty = true
		}
	}
	c.save()
}

// save writes the cache when it changed. It runs after every read, so what a
// tick that is killed mid-way had already shown is not shown again. A write
// that fails costs the next tick its reads and nothing else.
func (c *duplicateDetailCache) save() {
	if c.path == "" || !c.dirty {
		return
	}
	if err := atomicfile.EnsureDirAndWriteJSON(c.path, duplicateDetailFile{Owner: c.owner, Entries: c.entries}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: spec detail cache not saved: %v\n", err)
		return
	}
	c.dirty = false
}

func (e cachedDetail) detail() duplicateDetail {
	deps := make([]beads.IssueDep, len(e.Deps))
	for i, d := range e.Deps {
		deps[i] = beads.IssueDep{ID: d.ID, DependencyType: d.Type, Status: d.Status, CloseReason: d.CloseReason}
	}
	return duplicateDetail{
		Design:       e.Design,
		Notes:        e.Notes,
		Blockers:     blockingDepIDs(deps),
		OpenBlockers: beads.UnresolvedBlockerIDs(deps),
	}
}

func cachedDepsOf(deps []beads.IssueDep) []cachedDep {
	if len(deps) == 0 {
		return nil
	}
	out := make([]cachedDep, len(deps))
	for i, d := range deps {
		out[i] = cachedDep{ID: d.ID, Type: d.DependencyType, Status: d.Status, CloseReason: d.CloseReason}
	}
	return out
}
