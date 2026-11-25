package land

import (
	"context"
	"errors"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// AheadOutcome is what one review made ahead of a landing came to.
type AheadOutcome string

const (
	// AheadStored means the review's verdict was kept in the review-ahead
	// store. ReviewAhead does not return it yet.
	AheadStored AheadOutcome = "stored"
	// AheadConflict means the submission does not merge onto the target, so no
	// verdict was made. ReviewAhead does not return it yet.
	AheadConflict AheadOutcome = "conflict"
	// AheadSkipped is a submission no review was made for. The landing runs om
	// inline, as it does without a review-ahead store.
	AheadSkipped AheadOutcome = "skipped"
)

// ReviewAhead makes the review of w that a later landing reuses, ahead of
// that landing. It never writes to Beads: only the serial landing changes a
// bead. A nil Ahead store means the feature is off and nothing is read.
//
// The outcome is empty when err is set, so a caller that switches on it
// cannot mistake a failed read for a skipped review.
func (l *Lander) ReviewAhead(ctx context.Context, w Work) (AheadOutcome, error) {
	if l.Ahead == nil {
		return AheadSkipped, nil
	}
	issue, err := l.Beads.Show(w.BeadID)
	if err != nil {
		return "", &InfraError{Stage: "read bead", Err: err}
	}
	if issue == nil {
		return "", &InfraError{Stage: "read bead", Err: errors.New("no bead returned for " + w.BeadID)}
	}
	// The same readiness tests Land makes, in the same order, so a bead the
	// pool reviews is one the landing would not skip.
	if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() {
		return AheadSkipped, nil
	}
	if !beads.HasLabel(issue, LabelReadyToLand) {
		return AheadSkipped, nil
	}
	if policyRejection(issue) != nil {
		return AheadSkipped, nil
	}
	// The overseer reviewed this head in place of om, so it needs no review.
	if OverseerReviewed(issue, w.Head) {
		return AheadSkipped, nil
	}
	// NEXT BEAD: here ReviewAhead fetches the target and the branch under the
	// shared git lock and runs om on the head. Until that lands, every
	// submission that reaches this point is AheadSkipped and ctx is unused.
	return AheadSkipped, nil
}
