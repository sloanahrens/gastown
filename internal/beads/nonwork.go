package beads

import (
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// nonDispatchableTitlePrefixes are the families matched on title because a
// record can lose the type or label that would otherwise name it. Titles are
// the weaker signal: a family leaves this list once a type or label names it
// reliably.
//
// A prefix only belongs here when the family has no type or label that
// survives the record. HANDOFF and merge-slot have one: handoff mail carries
// gt:message and the merge slot carries gt:merge-slot, so the label check
// already catches them. A bare title match on those words would hide an
// ordinary bug that happens to start with the same word, so they are not
// listed. A compaction report is typed event, and stays because an event
// record that lost its type has only the title.
//
// A prefix that only announces a summary of work is deliberately absent. A
// "main_branch_test: <diagnosis>" bug and a "STATE_COLLAPSE <rig>" notice both
// read like alerts and the seat patrol suppresses both, but the patrol trades a
// missed nudge for silence and re-examines the board on its next tick, while a
// board that hides a filed bug hides work.
var nonDispatchableTitlePrefixes = []string{
	"Compaction Report ",
}

// IsNonDispatchableBead reports whether issue is work a polecat cannot take.
// It is NonDispatchableReason with the reason dropped.
func IsNonDispatchableBead(issue *Issue) bool {
	return NonDispatchableReason(issue) != ""
}

// NonDispatchableReason names why issue is not work a polecat can take, in the
// words a log or a board row shows ("type epic", "label gt:agent", "wisp",
// "title Compaction Report"). It returns "" for work. The spec dispatcher's
// NotWorkBead delegates here, so the board and the dispatcher cannot disagree
// about what work is.
//
// An epic is a container, so it is not work a polecat takes; its children are.
// A runtime family is named by type, by label, or, for a wisp, by ephemerality.
func NonDispatchableReason(issue *Issue) string {
	if issue == nil {
		return ""
	}

	t := strings.ToLower(strings.TrimSpace(issue.Type))
	if t == constants.EpicBeadType {
		return "type " + constants.EpicBeadType
	}
	for _, skip := range constants.NonDispatchableBeadTypes {
		if t == skip {
			return "type " + skip
		}
	}
	for _, label := range issue.Labels {
		for _, skip := range constants.NonDispatchableBeadLabels {
			if strings.EqualFold(strings.TrimSpace(label), skip) {
				return "label " + skip
			}
		}
	}
	if issue.Ephemeral {
		return "wisp"
	}

	title := strings.TrimSpace(issue.Title)
	for _, prefix := range nonDispatchableTitlePrefixes {
		if strings.HasPrefix(title, prefix) {
			return "title " + strings.TrimSpace(prefix)
		}
	}
	return ""
}
