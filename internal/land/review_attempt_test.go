package land

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestReviewAttemptLogLedgerAndHeaderAgree: a crew takeover's first review after
// three polecat refusals is round 1 in the reviews log, the rounds ledger and
// the refusal header (gt-df37yo).
func TestReviewAttemptLogLedgerAndHeaderAgree(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.reseat("crew/overseer/gt-x", "overseer")
	f.reseed(f.work, func(is *beads.Issue) {
		is.Notes += "\n" + laneBlocks("polecat/furiosa/gt-x+nonce", MaxReworkAttempts, RejectReview)
	})
	reviews := reviewsFileIn(t)
	l := f.lander()
	l.Reviews, l.RoundsDir = reviews, t.TempDir()
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictRequestChanges, Score: 0.5, Findings: []Finding{{ID: "F1", Severity: "major", Path: "b.txt", Line: 1, Title: "not atomic"}}}, nil
	}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the landing passed; want a review rejection")
	}
	recs, err := reviews.Since(time.Time{})
	rounds := renderedOf(t, &RoundsLedger{OutDir: l.RoundsDir, Bead: f.work.BeadID}, 0)
	header, ok := ParseRejectionNote(f.bead().Notes)
	if err != nil || len(recs) != 1 || len(rounds) != 1 || !ok {
		t.Fatalf("reviews log %d record(s) (%v), ledger %d round(s), header parsed %v; want one of each", len(recs), err, len(rounds), ok)
	}
	if recs[0].Attempt != 1 || rounds[0].Round != 1 || header.Attempt != 1 {
		t.Errorf("reviews log names %d, ledger %d, header %d; want round 1 in all three", recs[0].Attempt, rounds[0].Round, header.Attempt)
	}
}

// TestReviewAttemptNumbersTheRoundInItsLane: a review is numbered among the
// refusals in its own lane, so a re-dispatched polecat continues its count
// (gt-df37yo).
func TestReviewAttemptNumbersTheRoundInItsLane(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, notes, branch string
		attempt             int
	}{
		{"takeover after three polecat refusals", laneBlocks("polecat/furiosa/gt-x+nonce", MaxReworkAttempts, RejectReview), "crew/overseer/gt-x", 1},
		{"polecat re-dispatch after two refusals", laneBlocks("polecat/furiosa/gt-x+nonce", 2, RejectReview), "polecat/dusk/gt-x+other", 3},
		{"takeover after its own two refusals", laneBlocks("crew/overseer/gt-x", 2, RejectReview), "crew/overseer/gt-x", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := reviewAttempt(tc.notes, tc.branch); got != tc.attempt {
				t.Errorf("reviewAttempt(%s) = %d, want %d", tc.branch, got, tc.attempt)
			}
		})
	}
}
