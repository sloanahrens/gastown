package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/style"
)

// A sling that follows a landing rejection is a new round of work, and the
// per-bead respawn limit must not spend the bead on it (gt-tatcn). The limit is
// enforced in polecat_spawn.go, which clears a counted round here before it
// reads the count.

// submissionCommentRE matches both shapes gt done leaves on submission: the
// branch form, and the bare "Submitted for landing (attempt N)" a bead with no
// readable READY TO LAND block gets (done_agent_state.go).
var submissionCommentRE = regexp.MustCompile(`^Submitted for landing\b`)

// reworkRoundFrom reads from the bead and its comments whether the sling now
// being made follows a landing rejection — the bead carries the rework label
// the landing worker swaps in for gt:ready-to-land — and when the bead was
// last submitted for landing.
func reworkRoundFrom(issue *beads.Issue, comments []beads.Comment) (rework bool, submitted time.Time) {
	if issue == nil {
		return false, time.Time{}
	}
	return beads.HasLabel(issue, land.LabelRework), latestSubmissionTime(issue, comments)
}

// latestSubmissionTime is when a bead was last submitted for landing: the live
// READY TO LAND block's stamp (gt done writes it, gt-t2jhf), or, for a bead
// submitted before the block carried one, its newest submission comment. Zero
// is "no submission this code can date".
func latestSubmissionTime(issue *beads.Issue, comments []beads.Comment) time.Time {
	if w, ok := land.ParseReadyNote(issue.Notes); ok && !w.Submitted.IsZero() {
		return w.Submitted
	}
	for i := len(comments) - 1; i >= 0; i-- {
		if !submissionCommentRE.MatchString(strings.TrimSpace(comments[i].Text)) {
			continue
		}
		if at := beads.ParseIssueTime(comments[i].CreatedAt); !at.IsZero() {
			return at
		}
	}
	return time.Time{}
}

// clearRespawnForReworkRound clears beadID's respawn count when this sling
// follows a landing rejection, before the limit reads it. A read that fails
// clears nothing: an unreadable bead is no evidence of a submission, and
// keeping the count is the safe direction.
func clearRespawnForReworkRound(stores slingStores, townRoot, beadID string) {
	if beadID == "" {
		return
	}
	issue, err := stores.show(townRoot, beadID)
	if err != nil || issue == nil {
		return
	}
	comments, err := stores.comments(townRoot, beadID)
	if err != nil {
		comments = nil
	}
	rework, submitted := reworkRoundFrom(issue, comments)
	if !rework && submitted.IsZero() {
		return
	}
	cleared, err := polecat.ResetBeadRespawnForRound(townRoot, beadID, rework, submitted)
	if err != nil {
		style.PrintWarning("could not clear the respawn count for %s: %v", beadID, err)
		return
	}
	if cleared {
		fmt.Printf("  %s A rework round, not a respawn: cleared the count for %s\n", style.Bold.Render("✓"), beadID)
	}
}
