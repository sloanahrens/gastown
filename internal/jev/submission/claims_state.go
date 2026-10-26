package submission

import "github.com/steveyegge/gastown/internal/beads"

// ClaimsState is the state the claims_verified question reads for one bead and
// branch: the bead's fields, the commit subjects and the numstat, built the
// way the submission pass builds them. gt done uses it before submitting
// (gt-b9j3o.11).
func ClaimsState(issue *beads.Issue, subjects []string, numstat string) string {
	return buildState(issue, subjects, parseNumstat(numstat))
}
