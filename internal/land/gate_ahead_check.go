package land

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// gateAheadBeadCheck reports whether w's bead may be built ahead of its turn:
// the bead must be readable, actionable, carry the ready label and pass its own
// policy. A caller building a candidate ahead of its turn runs it first and
// logs the reason for anything else; it writes nothing, so the refusal is never
// recorded on the bead. The checks and their order are Land's own
// (precheckCar), so a bead gated ahead is one the landing would not skip or
// refuse (gt-vl8v7i).
func (l *Lander) gateAheadBeadCheck(w Work) error {
	issue, err := l.Beads.Show(w.BeadID)
	if err != nil {
		return fmt.Errorf("reading bead %s: %w", w.BeadID, err)
	}
	if issue == nil {
		return fmt.Errorf("reading bead %s: no bead returned", w.BeadID)
	}
	if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() {
		return fmt.Errorf("%w: %s is %s", ErrNotReady, w.BeadID, issue.Status)
	}
	if !beads.HasLabel(issue, LabelReadyToLand) {
		return fmt.Errorf("%w: %s has no %s label", ErrNotReady, w.BeadID, LabelReadyToLand)
	}
	if rej := policyRejection(issue); rej != nil {
		return fmt.Errorf("%w: %s: %s", ErrNotReady, w.BeadID, rej.Reason)
	}
	return nil
}
