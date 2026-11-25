package daemon

import "github.com/steveyegge/gastown/internal/polecat"

// seatMovedOn reports whether the seat's current branch names a bead other
// than the one that landed. The seat end keeps a landed record while the seat
// works its polecat branch rather than the branch that landed; a record whose
// seat has since been slung to another bead can never be satisfied, so the
// patrol drops it (gt-qrzhot, gt-i72us8).
//
// seatBranch is read in the generated polecat form
// polecat/<name>/<bead>+<suffix>, with the legacy @ separator accepted. A
// branch that names no bead (a crew branch, an empty string, the
// polecat/<name>-<suffix> form) and an empty landedBead are false. A branch
// naming the landed bead is false as well: a takeover's keep — "the seat works
// its polecat branch, not the crew branch that landed" — names the same bead
// and stays.
func seatMovedOn(seatBranch, landedBead string) bool {
	if landedBead == "" {
		return false
	}
	meta, ok := polecat.ParseBranchName(seatBranch)
	if !ok || meta.Issue == "" {
		return false
	}
	return meta.Issue != landedBead
}
