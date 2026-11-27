package cmd

import "fmt"

// raiseSizeRefusal escalates the size refusal gt done returned for bead, so
// the overseer is told by the run that was refused (gt-35xgjt). The fingerprint
// names the seat: a repeat by the same seat records onto its open escalation,
// and another seat raises its own. The refusal text is the reason, unchanged.
func raiseSizeRefusal(r escalateRun, bead, refusal string) error {
	r.severity = "medium"
	r.reason = refusal
	r.relatedBead = bead
	r.fingerprint = fmt.Sprintf("size-refusal:%s/%s/%s", r.rig, r.polecat, bead)
	r.stdin, r.dryRun, r.json = false, false, false
	return r.escalate([]string{fmt.Sprintf("%s: gt done refused this branch on size; the overseer decides whether to raise the limit or split the bead", bead)})
}
