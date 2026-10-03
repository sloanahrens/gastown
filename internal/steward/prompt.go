package steward

import (
	"fmt"
	"regexp"

	"github.com/steveyegge/gastown/internal/templates"
)

// PlanProposalMarker opens the block a plan job appends to a spec's notes:
// the child beads the planner proposes, which the operator files by hand. Its
// presence is what "planned" means — Detect raises no second plan event for a
// spec that carries one (gt-4k3fj.13).
//
// A plan job writes it in shadow mode too. The proposal is the deliverable,
// so what this kind's shadow mode withholds is filing the beads, not the note
// (gt-4k3fj.13).
const PlanProposalMarker = "PLAN PROPOSAL"

// planProposalRE matches the marker alone on its line. A proposal quotes the
// spec's own text, so a mention inside that prose is content, not a block —
// the rule the rejection blocks are read by (gt-4k3fj.13).
var planProposalRE = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(PlanProposalMarker) + `$`)

// HasPlanProposal reports whether notes carry a plan proposal.
func HasPlanProposal(notes string) bool { return planProposalRE.MatchString(notes) }

// PromptFor is the instruction one job runs: the event's facts, the procedure
// for its kind, what the job may touch, and the verdict file it must leave
// behind. final is true when no further job follows this one, which is what
// decides between handing a doubt to a stronger model and escalating it to
// the overseer (gt-9bioi.2).
//
// A plan job has no successor whatever model ran it, so the planner's prompt
// carries its own escalation rule and ignores final (gt-4k3fj.13).
func PromptFor(ev Event, final bool) (string, error) {
	t, err := templates.New()
	if err != nil {
		return "", err
	}
	text, err := t.RenderSteward(templates.StewardData{
		Kind: string(ev.Kind), Rig: ev.Rig, Bead: ev.Bead, Branch: ev.Branch,
		Head: ev.Head, Target: ev.Target, Worker: ev.Worker,
		Attempt: ev.Attempt, RejectionDetail: ev.RejectionDetail,
		ResultFile: ResultFile, Final: final, Shadow: ev.Mode.Shadow(),
		PlanMarker: PlanProposalMarker,
	})
	if err != nil {
		return "", fmt.Errorf("rendering the %s job's instructions for %s: %w", ev.Kind, ev.Bead, err)
	}
	return text, nil
}
