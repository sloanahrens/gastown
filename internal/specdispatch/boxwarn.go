package specdispatch

import "fmt"

// acceptanceWarnMin is the number of acceptance items a bead's list may carry
// before the lint warns. Measured on 2026-11-20 over the last 300 landings:
// beads with 5 or more acceptance items needed a rework round 78% of the time
// (n=18) against 33% over all landings, and 48% over the 721 before (n=21)
// against 25%. The warning changes neither the verdict nor the exit code.
const acceptanceWarnMin = 5

// acceptanceWarnText is the one WARN line a bead with acceptanceWarnMin or
// more acceptance items gets: the count and the split rule. It is a warning:
// the bead's route and exit code are unchanged.
const acceptanceWarnText = "%d acceptance items (beads with 5 or more needed a rework round about twice as often as the rest): consider splitting by concern"

// acceptanceWarnings returns the WARN lines s earns for a long acceptance
// list: one line when CountAcceptance(acceptanceText(s, sections)) is at
// acceptanceWarnMin or more and the bead does not wear the size-ok label the
// overseer waives a bead's size with, none otherwise. The waiver is the same
// one riskPathWarnings honors, since a bead the overseer has looked at and
// accepted is not warned twice about its size.
func acceptanceWarnings(s Spec, sections map[string]string) []string {
	if s.HasLabel(sizeOkLabel) {
		return nil
	}
	if n := CountAcceptance(acceptanceText(s, sections)); n >= acceptanceWarnMin {
		return []string{fmt.Sprintf(acceptanceWarnText, n)}
	}
	return nil
}
