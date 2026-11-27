package done

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// recordGateRun records a gate run that did not pass on the bead, so a load
// run can count it (gt-13dre6.17): "GATE RUN: " then the verdict, the wall and
// slot wait localGateNote renders, and the short head, and no gate output or
// error text. A missing client or a refused write warns; the gate's exit
// code and message are unchanged.
func (r *doneRun) recordGateRun(verdict string, res *land.GateResult, slotWait time.Duration, head string) {
	wall := time.Duration(0)
	if res != nil {
		for _, s := range res.Steps {
			wall += s.Elapsed
		}
	}
	comment := fmt.Sprintf("GATE RUN: %s %s, slot wait %s, head %s",
		verdict, wall.Round(time.Second), slotWait.Round(time.Second), ShortSHA(head))
	if r.sourceBD == nil {
		r.warnf("could not record the gate run on %s: no beads client", r.issueID)
		return
	}
	if err := r.sourceBD.AddCommentAs(r.issueID, r.sender, comment); err != nil {
		r.warnf("could not record the gate run on %s: %v", r.issueID, err)
	}
}
