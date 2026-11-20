package land

import (
	"context"
	"strings"
	"testing"
)

// roundsCapture records the rounds text the review is handed and answers with a verdict.
type roundsCapture struct {
	called  bool
	rounds  string
	verdict Verdict
}

// Review satisfies Reviewer; Land calls ReviewBead, which carries the rounds.
func (c *roundsCapture) Review(_ context.Context, _, _, _ string) (Verdict, error) {
	c.called = true
	return c.verdict, nil
}

func (c *roundsCapture) ReviewBead(_ context.Context, in ReviewInput) (Verdict, error) {
	c.called = true
	c.rounds = in.RoundsText
	return c.verdict, nil
}

// TestRoundsDegradeTheLandingOverARefusedLedger: an id-less finding makes the
// ledger one Render refuses (gt-2rvuze), and that refusal costs the landing its
// rounds, never the landing itself. roundsText returns the empty text, the bead
// is named once, and the review runs without rounds (gt-3r76hl).
func TestRoundsDegradeTheLandingOverARefusedLedger(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.RoundsDir = t.TempDir()
	// A finding with no id: om's ParseRounds requires one, so Render refuses
	// the ledger that records it.
	if err := (&RoundsLedger{OutDir: l.RoundsDir, Bead: f.work.BeadID}).
		Append(1, "head0", []Finding{{Path: "a.go", Title: "first"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	capture := &roundsCapture{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l.Reviewer = capture
	var out strings.Builder
	l.Out = &out
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if !capture.called {
		t.Fatal("the review never ran")
	}
	if capture.rounds != "" {
		t.Errorf("review rounds text = %q, want empty: a refused ledger hands om no rounds", capture.rounds)
	}
	if n := strings.Count(out.String(), "could not read the rounds ledger"); n != 1 {
		t.Errorf("the refused ledger is named %d time(s) in the landing log, want once:\n%s", n, out.String())
	}
}
