package done

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// followupBudgetError refuses a follow-up bead's submission whose diff is over
// the follow-up budget: a nit filed by the post-landing feed stays a nit
// instead of growing into a rework (gt-3der6d). A nil issue, a bead without
// land.FollowUpLabel, and a diff within both limits are not over budget.
func followupBudgetError(issue *beads.Issue, added, files, maxAdded, maxFiles int) error {
	if issue == nil || !beads.HasLabel(issue, land.FollowUpLabel) {
		return nil
	}
	if added <= maxAdded && files <= maxFiles {
		return nil
	}
	return fmt.Errorf("refusing to submit: this change adds %d lines in %d file(s), "+
		"over the diff budget of a follow-up bead (%d added lines, %d files)\n\n"+
		"A follow-up bead fixes one finding from a passing review, and a change past "+
		"its budget is a rework the finding did not ask for. Escalate the finding you "+
		"could not fix in budget instead of expanding the change:\n"+
		"  gt done --status ESCALATED\n"+
		"Name that finding in the bead's notes so the escalation carries it.",
		added, files, maxAdded, maxFiles)
}
