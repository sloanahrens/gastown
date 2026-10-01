package steward

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/templates"
)

// PromptFor is the instruction one job runs: the event's facts, the procedure
// for its kind, what the job may touch, and the verdict file it must leave
// behind. final is true when no further job follows this one, which is what
// decides between handing a doubt to a stronger model and escalating it to
// the overseer (gt-9bioi.2).
func PromptFor(ev Event, final bool) (string, error) {
	t, err := templates.New()
	if err != nil {
		return "", err
	}
	text, err := t.RenderSteward(templates.StewardData{
		Kind: string(ev.Kind), Rig: ev.Rig, Bead: ev.Bead, Branch: ev.Branch,
		Head: ev.Head, Target: ev.Target, Worker: ev.Worker,
		Attempt: ev.Attempt, RejectionDetail: ev.RejectionDetail,
		ResultFile: ResultFile, Final: final,
	})
	if err != nil {
		return "", fmt.Errorf("rendering the %s job's instructions for %s: %w", ev.Kind, ev.Bead, err)
	}
	return text, nil
}
