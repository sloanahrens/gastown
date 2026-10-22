package scorecard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
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
// gets it wrong therefore costs time, never a label. It says on stderr what it
// could not batch (warnf).
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
//
// The error is the batch calls that failed, joined, or a list that did not
// agree with a show; the map is still whatever could be read, and the caller
// says so with warnf and goes on. The list is checked against a show of one id
// because bd list prints fewer fields than bd show, and an adapter that read a
// field the list leaves out would label from an empty one.
func showAll(r any, ids []string) (map[string]*beads.Issue, error) {
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
		return nil, nil
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
	var errs []error
	readChunks := func(ids []string, read func([]string) (map[string]*beads.Issue, error)) {
		for len(ids) > 0 {
			n := min(showChunk, len(ids))
			got, err := read(ids[:n])
			if err != nil {
				errs = append(errs, err)
			}
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
		if err := checkListAgainstShow(r, found, want); err != nil {
			errs = append(errs, err)
			clear(found)
		}
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
	return found, errors.Join(errs...)
}

// checkListAgainstShow reads one listed bead the one-at-a-time way and compares
// the fields the adapters label from. The bead is the listed one with the most
// of those fields set, so a field the list leaves out shows as a difference
// whenever any listed bead has it. It returns nil when no bead was listed or
// the reader cannot show.
func checkListAgainstShow(r any, listed map[string]*beads.Issue, order []string) error {
	shower, ok := r.(beadShower)
	if !ok {
		return nil
	}
	var probe *beads.Issue
	best := -1
	for _, id := range order {
		issue := listed[id]
		if issue == nil {
			continue
		}
		if n := labelFieldsSet(issue); n > best {
			probe, best = issue, n
		}
	}
	if probe == nil {
		return nil
	}
	shown, err := shower.Show(probe.ID)
	if err != nil {
		return fmt.Errorf("checking bd list against bd show for %s: %w", probe.ID, err)
	}
	if field := firstLabelFieldDiff(probe, shown); field != "" {
		return fmt.Errorf("bd list and bd show disagree on the %s of %s", field, probe.ID)
	}
	return nil
}

// labelFields are the bead fields an adapter labels from: Status, CloseReason,
// Notes, Description, Priority and Labels (stale.go and dispatch.go read the last
// two). An adapter that reads another has to add it here.
func labelFields(i *beads.Issue) [6][2]string {
	return [6][2]string{
		{"status", i.Status},
		{"close reason", i.CloseReason},
		{"notes", i.Notes},
		{"description", i.Description},
		{"priority", strconv.Itoa(i.Priority)},
		{"labels", strings.Join(i.Labels, ",")},
	}
}

// labelFieldsSet counts the labeling fields i has set. Priority is formatted as
// a number, so an unset one reads "0" and would count; it counts only when it is
// not zero, or every bead would score one for a field it may not have.
func labelFieldsSet(i *beads.Issue) int {
	n := 0
	for _, f := range labelFields(i) {
		if f[1] != "" && (f[0] != "priority" || i.Priority != 0) {
			n++
		}
	}
	return n
}

// firstLabelFieldDiff names the first labeling field on which a and b differ,
// or "" when none does. A nil b differs on status.
func firstLabelFieldDiff(a, b *beads.Issue) string {
	if b == nil {
		return "status"
	}
	fa, fb := labelFields(a), labelFields(b)
	for i := range fa {
		if fa[i][1] != fb[i][1] {
			return fa[i][0]
		}
	}
	return ""
}

// warnf tells the operator on stderr that a batch read did not work and the
// report went on one read at a time. It is stderr so a --json report on stdout
// stays parseable.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gt jev scorecard: "+format+"\n", args...)
}

// itemIDs lists the ids of items.
func itemIDs(items []Item) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}
