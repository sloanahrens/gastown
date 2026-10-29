package land

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// A passing review is a score above the rig's bar and a set of findings that
// lets the work land: om scores a lone major inside the approving band, so the
// verdict alone would land one (gt-oi4je).

// findingsOf renders n findings of one severity, so a case stays a table row.
func findingsOf(severity string, n int) []Finding {
	out := make([]Finding, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Finding{ID: fmt.Sprintf("f-%d", i), Severity: severity, Path: "a.go", Line: i + 1, Title: "finding"})
	}
	return out
}

// TestFindingsRejection: the refusal an approving verdict earns from its
// findings, and the verdicts it leaves alone (gt-oi4je).
func TestFindingsRejection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		verdict Verdict
		want    string
	}{
		{
			name:    "an approve with no findings",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.90},
		},
		{
			name:    "an approve with one major",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.90, Findings: findingsOf("major", 1)},
			want:    "a major finding blocks the landing at any score",
		},
		{
			name:    "an approve with one critical",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.90, Findings: findingsOf("critical", 1)},
			want:    "a critical finding blocks the landing at any score",
		},
		{
			// Severity is compared the way the feed compares it.
			name:    "a padded, capitalised major",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.90, Findings: []Finding{{Severity: " Major ", Title: "leaks the token"}}},
			want:    "a major finding blocks the landing at any score",
		},
		{
			name: "a critical beside a major",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.90,
				Findings: append(findingsOf("major", 1), findingsOf("critical", 1)...)},
			want: "a critical finding blocks the landing at any score",
		},
		{
			name:    "an approve with five minors",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.76, Findings: findingsOf("minor", maxMinorFindings)},
		},
		{
			name:    "an approve with six minors",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.90, Findings: findingsOf("minor", maxMinorFindings+1)},
			want:    fmt.Sprintf("more than %d minor findings: fix the most important and resubmit", maxMinorFindings),
		},
		{
			// Info is what om already decided is not worth fixing, and it
			// fills no count of its own.
			name: "an approve with five minors beside info",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.90,
				Findings: append(findingsOf("minor", maxMinorFindings), findingsOf("info", 4)...)},
		},
		{
			// An unknown severity string does not block, and is not a minor.
			name:    "an approve with a severity the review does not know",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.90, Findings: findingsOf("blocker", maxMinorFindings+1)},
		},
		{
			name:    "an approve with an empty severity",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.90, Findings: []Finding{{Path: "a.go", Title: "no severity"}}},
		},
		{
			// A refusal is refused on its own terms, and the re-score above
			// keeps its findings to decide on.
			name:    "a request for changes with a major",
			verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.30, Findings: findingsOf("major", 1)},
		},
		{
			name:    "a skipped review",
			verdict: Verdict{Verdict: VerdictSkipped},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := findingsRejection(tc.verdict)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("findingsRejection = %q, want the findings to let it land", got.Reason)
				}
				return
			}
			if got == nil {
				t.Fatalf("findingsRejection = nil, want %q", tc.want)
			}
			if got.Reason != tc.want {
				t.Errorf("reason = %q, want %q", got.Reason, tc.want)
			}
			if got.Kind != RejectReview || !got.Rework {
				t.Errorf("refusal = %+v, want a %s refusal back to the author", got, RejectReview)
			}
		})
	}
}

// approveLanding installs a reviewer that answers every run with v.
func approveLanding(t *testing.T, f *landFixture, v Verdict) {
	t.Helper()
	f.review = &fakeReviewer{fn: func(string) (Verdict, error) { return v, nil }}
}

// TestLandRefusesAnApproveWithABlockingFinding: the score clears the rig's bar
// and the major refuses the landing anyway, back to the author with the reason
// and the finding on the bead (gt-oi4je).
func TestLandRefusesAnApproveWithABlockingFinding(t *testing.T) {
	t.Parallel()
	f := barFixture(t, "0.75")
	approveLanding(t, f, Verdict{Verdict: VerdictApprove, Score: 0.90,
		Findings: []Finding{{ID: "f-1", Severity: "major", Path: "b.txt", Line: 1, Title: "the write is not atomic"}}})

	_, err := f.lander().Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelRework)
	if !strings.Contains(rej.Reason, "a major finding blocks the landing at any score") {
		t.Errorf("rejection reason = %q, want the major named", rej.Reason)
	}
	if rej.ReviewScore != 0.90 || len(rej.Findings) != 1 || rej.Findings[0].ID != "f-1" {
		t.Errorf("rejection does not carry om's verdict: %+v", rej)
	}
	notes := f.bead().Notes
	if !strings.Contains(notes, "- id:f-1 sev:major b.txt:1 — the write is not atomic") {
		t.Errorf("the refusal does not carry the finding:\n%s", notes)
	}
	// The bead keeps om's own verdict beside the refusal: the review approved,
	// and the landing worker's finding rule is what refused it.
	comments, err := f.bd.Comments(f.work.BeadID)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 1 || !strings.Contains(comments[0].Text, "om approve 0.90") {
		t.Errorf("comments = %+v, want the one line recording om's approve", comments)
	}
}

// TestLandRefusesAnApproveWithATooManyMinors: six minors is where the review
// is saying the diff is not ready, whatever it scored (gt-oi4je).
func TestLandRefusesAnApproveWithTooManyMinors(t *testing.T) {
	t.Parallel()
	f := barFixture(t, "0.75")
	approveLanding(t, f, Verdict{Verdict: VerdictApprove, Score: 0.90, Findings: findingsOf("minor", maxMinorFindings+1)})

	_, err := f.lander().Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelRework)
	if !strings.Contains(rej.Reason, "more than 5 minor findings") {
		t.Errorf("rejection reason = %q, want the minor count named", rej.Reason)
	}
	if len(rej.Findings) != maxMinorFindings+1 {
		t.Errorf("rejection carries %d findings, want all %d so the author picks which to fix", len(rej.Findings), maxMinorFindings+1)
	}
}

// TestLandPassesAnApproveWithFiveMinorsAtTheBar: minors alone do not refuse a
// landing, and the rig's bar is still the score rule (gt-oi4je).
func TestLandPassesAnApproveWithFiveMinors(t *testing.T) {
	t.Parallel()
	f := barFixture(t, "0.75")
	approveLanding(t, f, Verdict{Verdict: VerdictApprove, Score: 0.76, Findings: findingsOf("minor", maxMinorFindings)})

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land = %v, want five minor findings to land", err)
	}
	if res.Verdict.Verdict != VerdictApprove || res.Verdict.Score != 0.76 {
		t.Errorf("verdict = %+v, want the reviewer's approve at 0.76", res.Verdict)
	}
}

// TestLandCapsTheFindingRefusal: a major that the author keeps shipping ends
// the rework loop like any other refusal, rather than redispatching forever
// (gt-oi4je).
func TestLandCapsTheFindingRefusal(t *testing.T) {
	t.Parallel()
	f := barFixture(t, "0.75")
	approveLanding(t, f, Verdict{Verdict: VerdictApprove, Score: 0.90, Findings: findingsOf("major", 1)})
	l := f.lander()
	for attempt := 1; attempt <= MaxReworkAttempts; attempt++ {
		_, err := l.Land(context.Background(), f.work)
		var rej *Rejection
		if !errors.As(err, &rej) || rej.Kind != RejectReview {
			t.Fatalf("attempt %d: Land = %T %v, want a review rejection", attempt, err, err)
		}
		if rej.RecordErr != nil {
			t.Fatalf("attempt %d: rejection not recorded: %v", attempt, rej.RecordErr)
		}
		capped := attempt >= MaxReworkAttempts
		if rej.Rework == capped {
			t.Errorf("attempt %d: rej.Rework = %v, want %v", attempt, rej.Rework, !capped)
		}
		b := f.bead()
		wantLabel := LabelRework
		if capped {
			wantLabel = LabelNeedsOverseer
		}
		if !beads.HasLabel(b, wantLabel) || CountRejections(b.Notes) != attempt {
			t.Errorf("attempt %d: labels = %v, %d rejection block(s); want %s and %d",
				attempt, b.Labels, CountRejections(b.Notes), wantLabel, attempt)
		}
		// The next round is the author's fresh submission of the same head.
		if err := f.bd.Update(f.work.BeadID, beads.UpdateOptions{AddLabels: []string{LabelReadyToLand}}); err != nil {
			t.Fatalf("attempt %d: re-submitting: %v", attempt, err)
		}
	}
	if b := f.bead(); !beads.HasLabel(b, LabelNeedsOverseer) {
		t.Errorf("after %d refusals the bead is %v, want %s", MaxReworkAttempts, b.Labels, LabelNeedsOverseer)
	}
}

// TestBlockingSeverity: the severities that refuse a landing whatever its
// score are the major and the critical, and nothing else (gt-oi4je).
func TestBlockingSeverity(t *testing.T) {
	t.Parallel()
	for _, severity := range []string{"major", "critical"} {
		f := []Finding{{Severity: severity}}
		if blockingSeverity(f) == "" || !hasMajorFinding(f) {
			t.Errorf("%q does not block", severity)
		}
	}
	for _, severity := range []string{"minor", "info", "blocker", ""} {
		f := []Finding{{Severity: severity}}
		if blockingSeverity(f) != "" || hasMajorFinding(f) {
			t.Errorf("%q blocks, and should not", severity)
		}
	}
}
