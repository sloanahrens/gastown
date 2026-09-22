package beads

import "strings"

// nonDispatchableIssueLabels are the bead families that carry a priority but no
// owner a polecat can take work from: an escalation waits on the mayor or the
// operator, a message on its recipient, an agent bead is a polecat's own
// identity, and a merge request is the refinery's queue.
//
// One list rather than one per caller, because the callers ask the same
// question and drifted while the lists were separate (gt-b9wq): the Ready panel
// offered Sling on mail beads and on the refinery's merge slot while the
// dashboard's Work panel filtered both.
var nonDispatchableIssueLabels = []string{
	"gt:agent",
	"gt:convoy",
	"gt:escalation",
	"gt:formula",
	"gt:handoff",
	"gt:keep",
	"gt:merge-request",
	"gt:merge-slot",
	"gt:message",
	"gt:queue",
	"gt:rig",
	"gt:role",
	"gt:standing-orders",
	"gt:wisp",
}

// nonDispatchableIssueTypes are the issue types that record town runtime rather
// than work — a message, a handoff note, an agent identity, and the deacon's
// event records (a compaction report, a reaper run).
//
// "event" is held here instead of in InternalIssueType because
// ConcreteWorkIssueRejectReason reads that predicate for a different question,
// where the answer for an event bead need not change.
var nonDispatchableIssueTypes = []string{
	"wisp",
	"message",
	"handoff",
	"merge-request",
	"agent",
	"queue",
	"convoy",
	"formula",
	"event",
}

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

	for _, skip := range nonDispatchableIssueTypes {
		if strings.EqualFold(strings.TrimSpace(issue.Type), skip) {
			return true
		}
	}
	for _, label := range issue.Labels {
		for _, skip := range nonDispatchableIssueLabels {
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
