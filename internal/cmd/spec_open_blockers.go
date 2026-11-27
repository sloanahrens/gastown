package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// openBlockerSet returns the direct and transitive open blockers of id as a set
// (creator cite 2/4). It walks breadth first, stops at maxBlockerDepth as
// blockerCreates does, and ends a cycle at a bead already walked. A read that
// fails, of id or of any blocker, is an error naming that bead and no set.
func openBlockerSet(read func(id string) (specdispatch.Spec, []string, error), id string) (map[string]bool, error) {
	_, blockers, err := read(id)
	if err != nil {
		return nil, fmt.Errorf("cannot read bead %s: %w", id, err)
	}
	type node struct {
		id    string
		depth int
	}
	seen := map[string]bool{id: true}
	queue := make([]node, 0, len(blockers))
	for _, blocker := range blockers {
		queue = append(queue, node{blocker, 1})
	}
	set := map[string]bool{}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.depth > maxBlockerDepth || seen[n.id] {
			continue
		}
		seen[n.id] = true
		_, blockers, err := read(n.id)
		if err != nil {
			return nil, fmt.Errorf("cannot read blocker %s: %w", n.id, err)
		}
		set[n.id] = true
		for _, blocker := range blockers {
			queue = append(queue, node{blocker, n.depth + 1})
		}
	}
	return set, nil
}
