package beads

import (
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// nonDispatchableTitlePrefixes are the unlabelled families, matched on title
// because no label distinguishes them. Titles are the weaker signal; a bead
// that gains a label can leave this list.
//
// A prefix that only announces a summary of work is deliberately absent. A
// "main_branch_test: <diagnosis>" bug and a "STATE_COLLAPSE <rig>" notice both
// read like alerts and the seat patrol suppresses both, but the patrol trades a
// missed nudge for silence and re-examines the board on its next tick, while a
// board that hides a filed bug hides work.
var nonDispatchableTitlePrefixes = []string{
	"HANDOFF",
	"merge-slot",
	"Compaction Report ",
}

// IsNonDispatchableBead reports whether issue belongs to a family that records
// town runtime rather than work someone can take.
//
// It judges family alone: priority is the caller's call, and so is any
// container the caller wants to keep. A convoy is named as a family and an epic
// is not, so an epic survives this predicate and the caller excludes it if the
// container itself is not what an agent works.
func IsNonDispatchableBead(issue *Issue) bool {
	if issue == nil {
		return false
	}

	for _, skip := range constants.NonDispatchableBeadTypes {
		if strings.EqualFold(strings.TrimSpace(issue.Type), skip) {
			return true
		}
	}
	for _, label := range issue.Labels {
		for _, skip := range constants.NonDispatchableBeadLabels {
			if strings.EqualFold(strings.TrimSpace(label), skip) {
				return true
			}
		}
	}

	title := strings.TrimSpace(issue.Title)
	for _, prefix := range nonDispatchableTitlePrefixes {
		if strings.HasPrefix(title, prefix) {
			return true
		}
	}
	return false
}
