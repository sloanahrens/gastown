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

// A sling that follows a round of work is not a respawn, and the per-bead
// respawn limit must not spend the bead on it (gt-tatcn). The limit is enforced
// in polecat_spawn.go, which clears the rounds behind a submission here before
// it reads the count.
//
// What dates a round is the submission, not the label. The landing worker's
// rework label records that a refusal happened, never when, so on its own it
// cannot say which recorded spawns came before the refusal: read as evidence by
// itself it cleared the count on every sling of a rework bead whose polecat
// kept dying at start, which is the case the limit exists for. The submission
// the refusal followed is the same event with a timestamp on it.

// submissionCommentRE matches both shapes gt done leaves on submission: the
// branch form, and the bare "Submitted for landing (attempt N)" a bead with no
// readable READY TO LAND block gets (done_agent_state.go).
var submissionCommentRE = regexp.MustCompile(`^Submitted for landing\b`)

// latestSubmissionTime is when a bead was last submitted for landing: the live
// READY TO LAND block's stamp (gt done writes it, gt-t2jhf), or, for a bead
// submitted before the block carried one, its newest submission comment. Zero
// is "no submission this code can date", which a nil bead is too.
func latestSubmissionTime(issue *beads.Issue, comments []beads.Comment) time.Time {
	if issue == nil {
		return time.Time{}
	}
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

// clearRespawnForSubmittedRound clears beadID's respawn count when this sling
// follows a round that reached a submission for landing, before the limit reads
// it. A bead whose submission cannot be read or dated clears nothing: the count
// is the breaker for a polecat that dies at start over and over, and keeping it
// is the safe direction.
func clearRespawnForSubmittedRound(stores slingStores, townRoot, beadID string) {
	if beadID == "" {
		return
	}
	issue, err := stores.show(townRoot, beadID)
	if err != nil || issue == nil {
		return
	}
	comments, err := stores.comments(townRoot, beadID)
	if err != nil {
		// Only the submission comments are lost. A bead whose READY TO LAND
		// block carries a stamp still dates its round, so this is reported and
		// not returned on.
		style.PrintWarning("could not read the comments on %s: %v", beadID, err)
		comments = nil
	}
	submitted := latestSubmissionTime(issue, comments)
	if submitted.IsZero() {
		return
	}
	cleared, err := polecat.ResetBeadRespawnForRound(townRoot, beadID, submitted)
	if err != nil {
		style.PrintWarning("could not clear the respawn count for %s: %v", beadID, err)
		return
	}
	if cleared {
		fmt.Printf("  %s Cleared the count for %s: it is a round that submitted, not a respawn\n", style.Bold.Render("✓"), beadID)
	}
}
