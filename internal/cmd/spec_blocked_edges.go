package cmd

import (
	"slices"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// blockedEdges is one rig's blocked-issue read, shaped for priority inheritance.
// Own maps each blocked issue to its own priority; BlockedBy maps it to the ids
// that block it. A ready blocker is on neither side, so its own priority is not
// in Own.
type blockedEdges struct {
	Own       map[string]int
	BlockedBy map[string][]string
}

// specBlockedMaps turns a rig's blocked issues into blockedEdges. Every blocked
// issue keeps its own priority, even one with no blockers, because a blocker
// inherits the priority of the dependent it holds up.
func specBlockedMaps(issues []*beads.Issue) blockedEdges {
	edges := blockedEdges{
		Own:       make(map[string]int, len(issues)),
		BlockedBy: make(map[string][]string, len(issues)),
	}
	for _, issue := range issues {
		// A nil entry carries no id, so there is nothing to record for it.
		if issue == nil {
			continue
		}
		edges.Own[issue.ID] = issue.Priority
		if len(issue.BlockedBy) > 0 {
			edges.BlockedBy[issue.ID] = issue.BlockedBy
		}
	}
	return edges
}

// specMergeBlockedEdges unions the per-rig edges into one blockedEdges. An id
// seen in more than one rig keeps the better (lower) priority, and a blocker
// listed for an id in several rigs appears once. The result never aliases the
// input slices, and no input gives empty non-nil maps.
func specMergeBlockedEdges(perRig []blockedEdges) blockedEdges {
	merged := blockedEdges{
		Own:       map[string]int{},
		BlockedBy: map[string][]string{},
	}
	for _, rig := range perRig {
		for id, priority := range rig.Own {
			if best, seen := merged.Own[id]; !seen || priority < best {
				merged.Own[id] = priority
			}
		}
		for id, blockers := range rig.BlockedBy {
			// An empty list carries no blocker, so it adds no edge, as in specBlockedMaps.
			if len(blockers) == 0 {
				continue
			}
			merged.BlockedBy[id] = appendNewIDs(merged.BlockedBy[id], blockers)
		}
	}
	return merged
}

// inheritedFromRigs gives each blocker the best priority of what it holds up,
// across every rig's blocked issues. perRig maps a rig name to that rig's
// blocked issues as bd reports them. A ready blocker is on no blocked list, so
// it is in no rig's Own map; it still gets an entry when a blocked issue it
// holds up has a known priority. The entry names that issue as its source.
func inheritedFromRigs(perRig map[string][]*beads.Issue) map[string]specdispatch.Inherited {
	edges := make([]blockedEdges, 0, len(perRig))
	for _, issues := range perRig {
		edges = append(edges, specBlockedMaps(issues))
	}
	merged := specMergeBlockedEdges(edges)
	return specdispatch.EffectivePriorities(merged.Own, merged.BlockedBy)
}

// appendNewIDs appends each id in ids that list does not already hold. Appending
// to a nil list allocates, so the result never shares a backing array with ids.
func appendNewIDs(list, ids []string) []string {
	for _, id := range ids {
		if !slices.Contains(list, id) {
			list = append(list, id)
		}
	}
	return list
}
