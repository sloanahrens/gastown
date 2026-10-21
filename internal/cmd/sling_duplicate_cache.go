package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// duplicateDetailCache keeps what that show returned, keyed by rig, bead id and
// the updated_at of the bd list row the tick already has. A bead whose row still
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
// list row still says UpdatedAt and DepCount. Its blocking edges are kept
// without status: a blocker closing does not touch the dependent's updated_at,
// so the status is read from the pool on every tick (openBlockersIn).
//
// DepCount is the row's dependency_count when the show ran. bd adds and removes
// a dependency without moving the dependent's updated_at, so updated_at alone
// would keep a bead's old edges until it is next edited, and the count is the
// only sign of an edge change the list row carries. It moves when an edge is
// added or removed; an edge swapped for another between two ticks leaves it
// the same, and the entry then lasts until the bead is next edited.
//
// An entry written before DepCount existed has no dep_count, which reads as 0.
// It is told apart by its edges: the count includes every edge the kept blocking
// ones came from, so an entry with edges and a count of 0 is one of those, and
// current treats it as stale rather than as a bead whose edges were all removed.
type cachedDetail struct {
	UpdatedAt string      `json:"updated_at"`
	DepCount  int         `json:"dep_count,omitempty"`
	Design    string      `json:"design,omitempty"`
	Notes     string      `json:"notes,omitempty"`
	Deps      []cachedDep `json:"deps,omitempty"`
}

// current reports whether the entry still describes the bead row: the same
// updated_at and dependency count, and a count that covers the edges kept.
func (e cachedDetail) current(row *beads.Issue) bool {
	if row.UpdatedAt == "" || e.UpdatedAt != row.UpdatedAt || e.DepCount != row.DependencyCount {
		return false
	}
	return e.DepCount > 0 || len(e.Deps) == 0
}

// cachedDep is a blocking dependency edge, without the target's status.
type cachedDep struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// duplicateDetailFile is the cache's file: each rig's pool details by bead id,
// for the daemon named by Owner.
type duplicateDetailFile struct {
	Owner string                             `json:"owner"`
	Rigs  map[string]map[string]cachedDetail `json:"rigs"`
}

// duplicateDetailCache is the daemon's design/notes cache for the duplicate
// check's pools. One value serves one tick: its counters are that tick's.
type duplicateDetailCache struct {
	limit int
	path  string // "" keeps the cache in memory
	owner string

	mu sync.Mutex
	// rigs holds each rig's entries, keyed by the rig's beads directory, so one
	// rig's pool fetch prunes only its own beads.
	rigs  map[string]map[string]cachedDetail
	dirty bool
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
		rigs:    map[string]map[string]cachedDetail{},
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
	if json.Unmarshal(data, &f) != nil || f.Owner != owner || f.Rigs == nil {
		return c
	}
	c.rigs = f.Rigs
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

// read recovers the detail of every row of the pool of the rig at beadsDir. A
// row the cache holds at the row's updated_at and dependency count is answered
// from it; the others are shown, up to the limit this tick has left, and the
// rest come back without a detail, counted in deferred. known is every row of
// the pool being built, which is what a cached blocker's status is read from.
// An error is the show's, and the details it returned are kept.
func (c *duplicateDetailCache) read(beadsDir string, store beads.Client, rows []*beads.Issue, known []*beads.Issue) (details map[string]duplicateDetail, deferred int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	status := make(map[string]*beads.Issue, len(known))
	for _, row := range known {
		status[row.ID] = row
	}
	entries := c.rigs[beadsDir]
	if entries == nil {
		entries = map[string]cachedDetail{}
		c.rigs[beadsDir] = entries
	}
	details = make(map[string]duplicateDetail, len(rows))
	var missing []*beads.Issue
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		entry, ok := entries[row.ID]
		if !ok || !entry.current(row) {
			missing = append(missing, row)
			continue
		}
		details[row.ID] = entry.detail(status)
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
			details[row.ID] = duplicateDetail{
				Design:       is.Design,
				Notes:        is.Notes,
				Blockers:     blockingDepIDs(is.Dependencies),
				OpenBlockers: beads.UnresolvedBlockerIDs(is.Dependencies),
			}
			if row.UpdatedAt == "" {
				continue
			}
			entries[row.ID] = cachedDetail{UpdatedAt: row.UpdatedAt, DepCount: row.DependencyCount, Design: is.Design, Notes: is.Notes, Deps: cachedDepsOf(is.Dependencies)}
			c.dirty = true
		}
	}
	c.save()
	return details, deferred, err
}

// retain drops the entries of the rig at beadsDir for beads its pool no longer
// lists, and saves. Other rigs' entries are not its to drop.
func (c *duplicateDetailCache) retain(beadsDir string, ids map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.rigs[beadsDir] {
		if !ids[id] {
			delete(c.rigs[beadsDir], id)
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
	// The file holds design and notes text, so only the daemon's user may read it.
	err := os.MkdirAll(filepath.Dir(c.path), 0o700)
	if err == nil {
		err = atomicfile.WriteJSONWithPerm(c.path, duplicateDetailFile{Owner: c.owner, Rigs: c.rigs}, 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: spec detail cache not saved: %v\n", err)
		return
	}
	c.dirty = false
}

// detail is the cached detail with its blockers read against status, the pool
// as it lists now.
func (e cachedDetail) detail(status map[string]*beads.Issue) duplicateDetail {
	deps := make([]beads.IssueDep, len(e.Deps))
	for i, d := range e.Deps {
		deps[i] = beads.IssueDep{ID: d.ID, DependencyType: d.Type}
	}
	return duplicateDetail{
		Design:       e.Design,
		Notes:        e.Notes,
		Blockers:     blockingDepIDs(deps),
		OpenBlockers: openBlockersIn(deps, status),
	}
}

// openBlockersIn is the blocking edges still open in the pool as it lists now.
// A target the pool does not list is resolved: an open bead of this rig is
// always listed, so an absent target is closed, or closed before the lookback.
// A merge-blocks edge is the exception: it resolves only on a close reason
// starting "Merged in ", which the pool no longer shows for an absent target,
// so its status is unknown and it stays unresolved (gt-idly3).
func openBlockersIn(deps []beads.IssueDep, status map[string]*beads.Issue) []string {
	listed := make([]beads.IssueDep, 0, len(deps))
	for _, d := range deps {
		row, ok := status[d.ID]
		if !ok {
			if strings.EqualFold(strings.TrimSpace(d.DependencyType), "merge-blocks") {
				d.Status, d.CloseReason = "", ""
				listed = append(listed, d)
			}
			continue
		}
		d.Status, d.CloseReason = row.Status, row.CloseReason
		listed = append(listed, d)
	}
	return beads.UnresolvedBlockerIDs(listed)
}

// cachedDepsOf keeps the blocking edges of a show, without their status.
func cachedDepsOf(deps []beads.IssueDep) []cachedDep {
	var out []cachedDep
	for _, d := range deps {
		if beads.IsBlockingDependencyType(d.DependencyType) {
			out = append(out, cachedDep{ID: d.ID, Type: d.DependencyType})
		}
	}
	return out
}
