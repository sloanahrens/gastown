package cmd

import (
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/land"
)

// The follow-up flow: the om-followup beads the post-landing feed filed
// (internal/land/followup.go). A bd store is one database, and the feed files a
// follow-up in the rig that landed the bead, so one pinned list would read the
// town's store alone while the rigs hold nearly all of them. The reader lists
// each store once — the town's ("hq") and one per rig — and joins the answers.
// This file is the issues half (gt-yy29ej); the landings read and the counts
// build on it.

// dashFollowupStores is the beads stores the flow reads: the town's own first,
// then each rig in name order. A registry that cannot be read is an error, not
// an empty rig list, which would silently drop the rigs from every count.
func dashFollowupStores(townRoot string) ([]string, error) {
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return nil, err
	}
	return append([]string{"hq"}, rigs...), nil
}

// followupAcrossStores joins read's answer for every store, in store order. A
// store that fails fails the whole read: an empty answer from one store would
// silently shrink every count drawn from the join.
func followupAcrossStores[T any](stores []string, read func(store string) ([]T, error)) ([]T, error) {
	var out []T
	for _, store := range stores {
		got, err := read(store)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// dashFollowupIssues lists every om-followup bead, open and closed, once per
// store. list is one store's bd list, injectable so the join is testable
// without a database; production passes none and lists through the store's
// beads directory.
func dashFollowupIssues(townRoot string, list ...func(store string) ([]*beads.Issue, error)) ([]*beads.Issue, error) {
	stores, err := dashFollowupStores(townRoot)
	if err != nil {
		return nil, err
	}
	read := func(store string) ([]*beads.Issue, error) {
		return beads.NewWithBeadsDir(townRoot, doltserver.FindRigBeadsDir(townRoot, store)).
			List(beads.ListOptions{Status: "all", Label: land.FollowUpLabel})
	}
	if len(list) > 0 && list[0] != nil {
		read = list[0]
	}
	return followupAcrossStores(stores, read)
}
