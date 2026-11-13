package land

import (
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land/policy"
)

// BuildReviewInput assembles every input one review of a bead's diff gets
// (gt-8af9s7): the single place a review's inputs are assembled, called by the
// landing worker and, next, by the polecat's local round, so the two cannot
// drift into judging the same bead against different material.
//
// issue is the work bead (the caller has read it) and comments are its
// comments; both are read by the caller, which also reads the accepted-risk
// text the bead records (AcceptedRisksText), the rounds ledger text
// (Lander.roundsText) and the operator identities (operator.names) the
// decision comments are entitled by. BuildReviewInput reads none of them
// itself: it does no I/O, so the two callers cannot disagree about whose
// comments, ledger or operator settings they gathered.
func BuildReviewInput(issue *beads.Issue, comments []beads.Comment, op *config.OperatorConfig, accepted, roundsText, base, head, dir string, repo Repo) ReviewInput {
	return ReviewInput{
		Dir: dir, Base: base, Head: head, Repo: repo,
		BeadID:            issue.ID,
		CriteriaText:      CriteriaText(issue),
		PriorFindings:     priorReviewFindings(issue.Notes),
		AcceptedRisksText: accepted,
		BriefText:         BeadBrief(issue.Description, comments, op),
		PolicyText:        policy.PolicyBlock(),
		Round:             reviewAttempt(issue.Notes),
		RoundsText:        roundsText,
	}
}
