package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// openBlockerSet returns the open blockers the read lists for id as a set.
// id is read once, and an id the read repeats is held once. A read that fails
// is an error naming the bead and no set: a caller must not answer a cite from
// a graph it could not read (creator cite 2/4).
func openBlockerSet(read func(id string) (specdispatch.Spec, []string, error), id string) (map[string]bool, error) {
	_, blockers, err := read(id)
	if err != nil {
		return nil, fmt.Errorf("cannot read bead %s: %w", id, err)
	}
	set := make(map[string]bool, len(blockers))
	for _, blocker := range blockers {
		set[blocker] = true
	}
	return set, nil
}
