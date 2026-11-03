package cmd

import (
	"fmt"
	"sort"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// maxBlockerDepth bounds the blocker walk: direct blockers are depth 1, and a
// chain deeper than this is not a plan one path check can resolve. A cycle is
// ended by the visited set, not by this cap.
const maxBlockerDepth = 8

// blockerCreates returns the sorted, unique repo paths the open transitive
// blockers of id declare they create in their "## Files" sections (gt-hgt9j.12).
// read returns one bead's spec and the ids of its open blockers, so a closed
// blocker is never walked: it is not in the list read answers.
//
// The walk is breadth first and visits each id once, so a cycle ends. A bead
// whose own description has no Files section answers nil without a blocker
// read: the path check consults this set only for a bead with a Files section,
// and a bead without one has nothing for a blocker's create to pass. A blocker
// with no Files section adds nothing and is not an error.
//
// A read that fails is an error naming the bead it was made for, and a
// blocker's Files section that does not parse is an error naming that blocker.
// Either way there is no set: an unreadable blocker is never read as an empty
// one, since an empty set would refuse a path a blocker really creates, and the
// caller must hold the bead rather than answer it from a graph it could not
// read.
func blockerCreates(read func(id string) (specdispatch.Spec, []string, error), id string) ([]string, error) {
	spec, blockers, err := read(id)
	if err != nil {
		return nil, fmt.Errorf("cannot read bead %s: %w", id, err)
	}
	if _, present, err := specdispatch.ParseFiles(spec.Description); err != nil || !present {
		return nil, nil
	}
	// Each queued id carries the depth it was reached at, so the cap is the
	// graph's distance from id and not the queue's length.
	type node struct {
		id    string
		depth int
	}
	seen := map[string]bool{id: true}
	queue := make([]node, 0, len(blockers))
	for _, blocker := range blockers {
		queue = append(queue, node{id: blocker, depth: 1})
	}
	set := map[string]bool{}
	var creates []string
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.depth > maxBlockerDepth || seen[n.id] {
			continue
		}
		seen[n.id] = true
		spec, blockers, err := read(n.id)
		if err != nil {
			return nil, fmt.Errorf("cannot read blocker %s: %w", n.id, err)
		}
		block, present, err := specdispatch.ParseFiles(spec.Description)
		if err != nil {
			return nil, fmt.Errorf("blocker %s: %w", n.id, err)
		}
		if present {
			for _, path := range block.Paths(specdispatch.FileCreate) {
				if !set[path] {
					set[path] = true
					creates = append(creates, path)
				}
			}
		}
		for _, blocker := range blockers {
			queue = append(queue, node{id: blocker, depth: n.depth + 1})
		}
	}
	sort.Strings(creates)
	return creates, nil
}

// specBlockerRead is blockerCreates' read over bd: a bead's spec and the ids of
// its open blocking dependencies. read is the bare bd show, and reads is the
// tick's ledger, nil outside a tick; a bead the tick already read is answered
// from the ledger, so the walk costs a bd call only for a blocker the tick has
// not seen.
func specBlockerRead(reads *specTickReads, read func(id string) (*beads.Issue, error)) func(id string) (specdispatch.Spec, []string, error) {
	return func(id string) (specdispatch.Spec, []string, error) {
		issue, err := reads.show(id, func() (*beads.Issue, error) { return read(id) })
		if err != nil {
			return specdispatch.Spec{}, nil, err
		}
		return specFromIssue(issue), beads.UnresolvedBlockerIDs(issue.Dependencies), nil
	}
}

// specWithBlockerCreates fills spec.BlockerCreates from the open transitive
// blockers of its bead (gt-hgt9j.12). A bead with no Files section, or with one
// that does not parse, asks nothing of the graph: the lint refuses a malformed
// Files section on its own and never reads this set for it. An error is
// returned unchanged so the caller can refuse the bead with it as the reason.
func specWithBlockerCreates(spec specdispatch.Spec, read func(id string) (specdispatch.Spec, []string, error), beadID string) (specdispatch.Spec, error) {
	if _, present, err := specdispatch.ParseFiles(spec.Description); !present || err != nil {
		return spec, nil
	}
	creates, err := blockerCreates(read, beadID)
	if err != nil {
		return spec, err
	}
	spec.BlockerCreates = creates
	return spec, nil
}
