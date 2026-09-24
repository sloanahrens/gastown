package constants

// Bead families that record town runtime rather than work a polecat can take
// from them. One list shared by every consumer that asks the same question
// (the Ready panel, the dashboard Work panel, the sling backpressure guard,
// the dispatch patrol): the families are defined once here, and a consumer
// that needs one of the two halves can ask for it — the durable issue types
// and labels for bd filters and client-side checks — without keeping its own
// drifting copy (gt-b9wq: the callers asked the same question and drifted
// while the lists were separate; gt-0q80: moved here so the store's ready
// query can carry the same exclusions server-side that the CLI path already
// sends).
//
// The durable types and the labels are the same families expressed in two
// bead kinds: a bead's issue_type and its label both carry the family
// ("gt:agent" is the label an agent bead wears; "message" is the issue_type
// mail beads can be typed with even when the label is the only signal).

// NonDispatchableBeadTypes are the bead kinds that record town runtime — a
// message, a handoff note, an agent identity, and the deacon's event records
// (a compaction report, a reaper run).
//
// "event" is here rather than in BeadsInfraTypesList because that list
// answers a different question: a consumer that filters on the infra types
// alone misses the runtime beads that lost their type and now read as plain
// tasks wearing a "gt:" label.
var NonDispatchableBeadTypes = []string{
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

// NonDispatchableBeadLabels are the "gt:" labels that mark a bead as a member
// of a runtime family, regardless of the issue_type it carries. An escalation
// waits on the mayor or the operator, a message on its recipient, an agent
// bead is a polecat's own identity, and a merge request is the refinery's
// queue: none of them carries work a polecat can take.
//
// Mail is the representative case: a mail bead is typed "task" (so no type
// filter reaches it) and is distinguished only by its "gt:message" label.
var NonDispatchableBeadLabels = []string{
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

// NonDispatchableBeadWispTypes are the NonDispatchableBeadTypes, excluding
// "wisp" itself: the store's wisp scan walks the wisps table, where every row
// is by definition a wisp, so the entry for the wisp kind would exclude the
// entire scan (gt-0q80).
func NonDispatchableBeadWispTypes() []string {
	out := make([]string, 0, len(NonDispatchableBeadTypes))
	for _, t := range NonDispatchableBeadTypes {
		if t != "wisp" {
			out = append(out, t)
		}
	}
	return out
}
