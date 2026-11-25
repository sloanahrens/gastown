package cmd

import "github.com/steveyegge/gastown/internal/beads"

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
