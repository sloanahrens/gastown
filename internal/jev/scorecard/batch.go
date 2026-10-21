package scorecard

import (
	"context"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// Prefetcher is an Adapter that can load what Label will read for a whole
// window of items at once, so a report costs a few subprocess calls and not
// one or more per item. Build calls Prefetch once, after the log is read and
// before the first Label.
//
// Prefetch is best effort and returns nothing: whatever it could not load,
// Label reads on its own and fails on as it always did. A Prefetch that
// gets it wrong therefore costs time, never a label.
type Prefetcher interface {
	Prefetch(ctx context.Context, items []Item)
}

// Bulky is an Adapter whose item rows are too many to print by default: the
// --json report leaves Lines out unless asked, and keeps its totals.
type Bulky interface {
	BulkyLines()
}

// beadBatcher reads several beads in one call. *beads.Beads is one.
type beadBatcher interface {
	ShowMultiple(ids []string) (map[string]*beads.Issue, error)
}

// beadLister reads several beads of one store in one query, much faster than a
// batch of shows. A beads.Client is read through beads.ListByIDs; a reader
// that is not one but has this method is read through it.
type beadLister interface {
	ListByIDs(ids []string) (map[string]*beads.Issue, error)
}

// showChunk bounds how many ids go on one bd command line.
const showChunk = 100

// showAll reads the beads named ids from r in as few calls as it can: one list
// query per chunk, then a batched show for what the query did not return (an
// id in another store, or a wisp). A reader that cannot batch yields nothing,
// and so does an id no batch found or a call that failed: the caller reads
// those one at a time, which gives back the exact error or not-found answer a
// single read does.
func showAll(r any, ids []string) map[string]*beads.Issue {
	var list func([]string) (map[string]*beads.Issue, error)
	switch v := r.(type) {
	case beadLister:
		list = v.ListByIDs
	case beads.Client:
		list = func(ids []string) (map[string]*beads.Issue, error) { return beads.ListByIDs(v, ids) }
	}
	canList := list != nil
	batcher, canBatch := r.(beadBatcher)
	if !canList && !canBatch {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	var want []string
	for _, id := range ids {
		// An id that starts with "-" could read as a bd option; the single read
		// handles it as it always has.
		if id == "" || strings.HasPrefix(id, "-") || seen[id] {
			continue
		}
		seen[id] = true
		want = append(want, id)
	}
	found := make(map[string]*beads.Issue, len(want))
	readChunks := func(ids []string, read func([]string) (map[string]*beads.Issue, error)) {
		for len(ids) > 0 {
			n := min(showChunk, len(ids))
			got, _ := read(ids[:n])
			for id, issue := range got {
				if issue != nil {
					found[id] = issue
				}
			}
			ids = ids[n:]
		}
	}
	if canList {
		readChunks(want, list)
	}
	if canBatch {
		var rest []string
		for _, id := range want {
			if found[id] == nil {
				rest = append(rest, id)
			}
		}
		readChunks(rest, batcher.ShowMultiple)
	}
	return found
}

// itemIDs lists the ids of items.
func itemIDs(items []Item) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}
