package land

import (
	"context"
	"io"
	"os"
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

// TestLogRoundsKeepsThePolecatRoundsUnderATakeover: a takeover's first review
// is its lane's round 1, added beside the polecat's rounds (gt-p28de8).
func TestLogRoundsKeepsThePolecatRoundsUnderATakeover(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	//testpolicy:allow no-exec-files — a directory, not a file: t.TempDir is 0777 under the runner's open umask, so it is made private
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger := RoundsLedger{OutDir: dir, Bead: "gt-x"}
	for i, head := range []string{"p1", "p2", "p3"} {
		must(t, ledger.Append(i+1, head, nil))
	}
	l := &Lander{RoundsDir: dir, Out: io.Discard}
	issue := &beads.Issue{ID: "gt-x", Notes: laneBlocks("polecat/furiosa/gt-x+nonce", 3, RejectReview)}
	l.logRounds(issue, Work{BeadID: "gt-x", Branch: "crew/overseer/gt-x", Head: "crewhead"},
		Verdict{Verdict: VerdictRequestChanges, Findings: []Finding{{ID: "F1", Severity: "major"}}})
	rounds := renderedOf(t, &RoundsLedger{OutDir: dir, Bead: "gt-x"}, 0)
	if len(rounds) != 4 || rounds[0].Head != "p1" || rounds[1].Head != "p2" || rounds[2].Head != "p3" || rounds[3].Head != "crewhead" {
		t.Fatalf("rounds = %+v, want the polecat's rounds kept and the crew's round 1 added", rounds)
	}
	// The findings stay with the round that raised them: the polecat's rounds
	// carry none, and the takeover's round carries its F1.
	for i := 0; i < 3; i++ {
		if len(rounds[i].Findings) != 0 {
			t.Errorf("polecat round %d carries %+v, want no findings", i+1, rounds[i].Findings)
		}
	}
	if len(rounds[3].Findings) != 1 || rounds[3].Findings[0].ID != "F1" {
		t.Errorf("the takeover's round carries %+v, want its F1", rounds[3].Findings)
	}
	// A polecat landing's lane is "", so it replaces its own round 1.
	l.logRounds(&beads.Issue{ID: "gt-x"}, Work{BeadID: "gt-x", Branch: "polecat/nux/gt-x+nonce", Head: "p1b"},
		Verdict{Verdict: VerdictRequestChanges})
	if rounds := renderedOf(t, &RoundsLedger{OutDir: dir, Bead: "gt-x"}, 0); len(rounds) != 4 || rounds[0].Head != "p1b" {
		t.Errorf("a polecat round rendered %+v, want the polecat lane's round 1 replaced", rounds)
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
