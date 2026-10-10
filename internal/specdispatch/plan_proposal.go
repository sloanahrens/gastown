package specdispatch

import "regexp"

// PlanProposalMarker opens the block a planner appends to a spec's notes: the
// child beads it proposes, which the operator files by hand. Its presence is
// what "planned" means — the dispatcher holds the spec as plan proposed from
// then on and files nothing from the block (gt-4k3fj.13, gt-4k3fj.14).
const PlanProposalMarker = "PLAN PROPOSAL"

// planProposalRE matches the marker alone on its line. A proposal quotes the
// spec's own text, so a mention inside that prose is content, not a block —
// the rule the rejection blocks are read by (gt-4k3fj.13).
var planProposalRE = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(PlanProposalMarker) + `$`)

// HasPlanProposal reports whether notes carry a plan proposal.
func HasPlanProposal(notes string) bool { return planProposalRE.MatchString(notes) }
