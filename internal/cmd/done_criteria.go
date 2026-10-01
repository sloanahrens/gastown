package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// refuseUncheckedCriteria refuses a submission whose work bead still has
// unchecked acceptance criteria, naming each one. The landing worker rejects
// such a bead as policy (land.policyRejection counts them with the same
// beads.UncheckedCriteria), but by then the session has retired; refusing
// here tells the author while the session is up. nil when every box is
// ticked or the bead has no criteria. It does not judge whether the criteria
// are met.
func refuseUncheckedCriteria(issueID string, issue *beads.Issue) error {
	unchecked := beads.UncheckedCriteria(issue)
	if len(unchecked) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "cannot submit: %s has %d unchecked acceptance criteria, and the landing worker rejects a bead with any:\n", issueID, len(unchecked))
	for _, line := range unchecked {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	fmt.Fprintf(&b, "Tick each one your work satisfies: bd show %s prints the criteria; resend the whole block with '- [x]' on each satisfied line:\n", issueID)
	fmt.Fprintf(&b, "  gt bead update %s --acceptance=\"<the block, those lines ticked>\"\n", issueID)
	fmt.Fprintf(&b, "Then run gt done again. A criterion the work does not satisfy is unfinished work: finish it, or gt escalate.")
	return fmt.Errorf("%s", b.String())
}
