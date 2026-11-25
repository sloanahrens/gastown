package land

import (
	"context"
	"errors"
	"strings"
	"time"

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
	// started is when this review began: the land log dates it now, and a later
	// bead stores it as the record's started_at. Nothing stores a record yet.
	started := l.aheadNow()
	l.logf("%s: ahead review of %s started at %s", w.BeadID, shortSHA(w.Head), started.UTC().Format(time.RFC3339))
	if err := l.aheadFetches(l.open(l.Repo), w); err != nil {
		return "", err
	}
	// NEXT BEAD: here ReviewAhead takes its own worktree at base and runs om on
	// the head. Until that lands, every submission that reaches this point is
	// AheadSkipped and ctx is unused.
	return AheadSkipped, nil
}

// aheadNow is the clock a review is dated by: the store's clock when it sets
// one, the wall clock otherwise, as AheadStore documents.
func (l *Lander) aheadNow() time.Time {
	if l.Ahead != nil && l.Ahead.Now != nil {
		return l.Ahead.Now()
	}
	return time.Now()
}

// aheadFetches brings the target and the work branch into the lander's clone,
// the two fetches Land makes before it checks the head out. They run under the
// shared git lock, which the deferred unlock releases on every return path and
// on a panic's unwind. The lock lives here, not in ReviewAhead, because it is
// not reentrant and the worktree add a later bead makes after the fetches takes
// it for itself.
//
// A submission the landing would refuse as not pushed has no head to review, so
// its rejection is returned as an error: the pool writes no bead and rejects
// nothing, and the serial landing's own check runs the rejection for real.
func (l *Lander) aheadFetches(g Repo, w Work) error {
	l.gitMu.Lock()
	defer l.gitMu.Unlock()
	if err := l.fetchTarget(g, w.Target); err != nil {
		return &InfraError{Stage: "fetch target", Err: err}
	}
	rej, err := l.checkHeadPushed(g, w)
	if err != nil {
		return err
	}
	if rej != nil {
		return rej
	}
	return nil
}
