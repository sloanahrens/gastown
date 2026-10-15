package land

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Reviewer noise decides landings near the bar: the same head scored 0.78 in
// one om run and 0.80 in another, and 0.76 against 0.72 in another pair. A
// rejection with no major finding that scored within rescoreMargin of the bar is
// therefore re-scored once on the same head, and the landing stands on the
// higher of the two scores (gt-cz07b).

// barFixture is the land fixture under a rig's review bar: .om.json rides the
// target and the work branch branches from it, so the merged tree the review and
// the re-score decision read the bar from carries it, as every real landing's
// does.
func barFixture(t *testing.T, threshold string) *landFixture {
	t.Helper()
	f := newLandFixture(t)
	f.base = f.git.Commit(t, f.origin, "main", "chore: the rig's review bar",
		map[string]string{".om.json": `{"threshold": ` + threshold + `}`})
	f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
	f.ready(f.git.Commit(t, f.origin, fixtureBranch, "feat: add b", map[string]string{"b.txt": "work\n"}))
	return f
}

// reviewStep is one answer a sequenceReviewer gives: a verdict, or the error a
// run that reached none reports.
type reviewStep struct {
	verdict Verdict
	err     error
}

// sequenceReviewer answers each review with the next step and records the input
// it was given. A review past the end of its steps fails the test: how many
// times om ran is what the re-score rules are about (gt-cz07b).
type sequenceReviewer struct {
	t      *testing.T
	steps  []reviewStep
	inputs []ReviewInput
}

func (r *sequenceReviewer) Review(_ context.Context, dir, base, head string) (Verdict, error) {
	return r.next(ReviewInput{Dir: dir, Base: base, Head: head})
}

func (r *sequenceReviewer) ReviewBead(_ context.Context, in ReviewInput) (Verdict, error) {
	return r.next(in)
}

func (r *sequenceReviewer) next(in ReviewInput) (Verdict, error) {
	r.inputs = append(r.inputs, in)
	i := len(r.inputs) - 1
	if i >= len(r.steps) {
		r.t.Fatalf("review %d ran; the reviewer was given %d answer(s)", i+1, len(r.steps))
	}
	return r.steps[i].verdict, r.steps[i].err
}

// TestBorderlineRejection: which rejections earn a second run (gt-cz07b).
func TestBorderlineRejection(t *testing.T) {
	t.Parallel()
	minor := []Finding{{Severity: "minor", Path: "a.go", Line: 1, Title: "unclear"}}
	for _, tc := range []struct {
		name    string
		verdict Verdict
		bar     float64
		reject  bool
	}{
		{name: "0.78 against a 0.80 bar", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.78, Findings: minor}, bar: 0.8, reject: true},
		{name: "exactly the margin below the bar", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.77, Findings: minor}, bar: 0.8, reject: true},
		{name: "past the margin", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.769, Findings: minor}, bar: 0.8},
		{name: "at the bar, not below it", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.80, Findings: minor}, bar: 0.8},
		{name: "above the bar", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.82, Findings: minor}, bar: 0.8},
		{name: "a major finding", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.78,
			Findings: []Finding{{Severity: "major", Path: "a.go", Line: 1, Title: "leaks the token"}}}, bar: 0.8},
		{name: "a critical finding", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.78,
			Findings: []Finding{{Severity: "Critical", Path: "a.go", Line: 1, Title: "leaks the token"}}}, bar: 0.8},
		{name: "no findings at all", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.78}, bar: 0.8, reject: true},
		{name: "a severity om does not score high", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.78,
			Findings: []Finding{{Severity: "info", Path: "a.go", Line: 1, Title: "a thought"}}}, bar: 0.8, reject: true},
		{name: "no bar in the tree", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.02, Findings: minor}},
		{name: "an approve", verdict: Verdict{Verdict: VerdictApprove, Score: 0.78, Findings: minor}, bar: 0.8},
		{name: "a skipped review", verdict: Verdict{Verdict: VerdictSkipped}, bar: 0.8},
		{name: "a review that did not run", verdict: Verdict{Verdict: VerdictErrorPrefix + "om review did not run"}, bar: 0.8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := borderlineRejection(tc.verdict, tc.bar); got != tc.reject {
				t.Errorf("borderlineRejection(%s %.2f, bar %.2f) = %v, want %v", tc.verdict.Verdict, tc.verdict.Score, tc.bar, got, tc.reject)
			}
		})
	}
}

// TestLandRescoresABorderlineRejection: a rejection within the margin of the bar
// with no major finding runs om once more on the same head, and the higher score
// decides: the landing lands, and the verdict comment carries both scores
// (gt-cz07b).
func TestLandRescoresABorderlineRejection(t *testing.T) {
	t.Parallel()
	f := barFixture(t, "0.8")
	rev := &sequenceReviewer{t: t, steps: []reviewStep{
		{verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.78, Version: "v1.2.3", Findings: []Finding{
			{ID: "f-1", Severity: "minor", Path: "a.go", Line: 4, Title: "the name is vague"},
			{ID: "f-2", Severity: "minor", Path: "b.go", Line: 9, Title: "the comment repeats the code"},
		}}},
		{verdict: Verdict{Verdict: VerdictApprove, Score: 0.80, Version: "v1.2.3"}},
	}}
	l := f.lander()
	l.Reviewer = rev
	var log strings.Builder
	l.Out = &log

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v; a re-scored rejection that approved must land", err)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want the landed commit %s", got, res.LandedCommit)
	}
	if len(rev.inputs) != 2 {
		t.Fatalf("om ran %d time(s), want 2", len(rev.inputs))
	}
	if rev.inputs[0].Head != rev.inputs[1].Head || rev.inputs[0].Base != rev.inputs[1].Base {
		t.Errorf("the rerun reviewed %s..%s, want the same head as the first run %s..%s",
			rev.inputs[1].Base, rev.inputs[1].Head, rev.inputs[0].Base, rev.inputs[0].Head)
	}
	if !strings.Contains(log.String(), "0.78") || !strings.Contains(log.String(), "0.80") {
		t.Errorf("the landing log does not name both scores:\n%s", log.String())
	}

	var rec LandingRecord
	if err := json.Unmarshal([]byte(f.landingLines()[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.OMVerdict != VerdictApprove || rec.OMScore != 0.80 {
		t.Errorf("landing record = %q at %.2f, want the higher-scoring run approve at 0.80", rec.OMVerdict, rec.OMScore)
	}
	comments := f.beadComments()
	if len(comments) != 1 {
		t.Fatalf("comments = %d (%q), want exactly one", len(comments), commentTexts(comments))
	}
	got := parseVerdictComment(t, comments[0].Text)
	if got.word != VerdictApprove || got.score != "0.80" {
		t.Errorf("verdict comment = %q, want the approve at 0.80", comments[0].Text)
	}
	if !strings.Contains(got.rescore, "0.78") || !strings.Contains(got.rescore, "0.80") {
		t.Errorf("re-score clause = %q, want both scores", got.rescore)
	}
}

// TestLandTakesTheHigherScoreOfTwoRejections: a rerun that also rejects can
// still be the better of the two, and the refusal carries the score the landing
// stood on (gt-cz07b).
func TestLandTakesTheHigherScoreOfTwoRejections(t *testing.T) {
	t.Parallel()
	f := barFixture(t, "0.8")
	rev := &sequenceReviewer{t: t, steps: []reviewStep{
		{verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.78, Findings: []Finding{
			{ID: "f-1", Severity: "minor", Path: "a.go", Line: 4, Title: "the name is vague"}}}},
		{verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.79, Findings: []Finding{
			{ID: "f-2", Severity: "minor", Path: "a.go", Line: 6, Title: "the branch repeats the condition"}}}},
	}}
	l := f.lander()
	l.Reviewer = rev

	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectReview, LabelRework)
	if len(rev.inputs) != 2 {
		t.Fatalf("om ran %d time(s), want 2", len(rev.inputs))
	}
	last, ok := ParseRejectionNote(f.bead().Notes)
	if !ok {
		t.Fatalf("no rejection block in the notes:\n%s", f.bead().Notes)
	}
	if last.Attempt != 1 {
		t.Errorf("attempt = %d, want 1: a re-score is not a rework attempt", last.Attempt)
	}
	if last.Receipt == nil || last.Receipt.Score != 0.79 {
		t.Errorf("receipt = %+v, want the higher score 0.79", last.Receipt)
	}
	got := parseVerdictComment(t, commentTexts(f.beadComments())[0])
	if got.word != VerdictRequestChanges || got.score != "0.79" {
		t.Errorf("verdict comment = %q, want the request for changes at 0.79", commentTexts(f.beadComments())[0])
	}
	if !strings.Contains(got.rescore, "0.78") {
		t.Errorf("re-score clause = %q, want the rejected run's score too", got.rescore)
	}
}

// TestLandDoesNotRescoreARejectionWithAMajor: a major finding is om's judgment
// about the diff, not noise about the score, so the refusal stands on one run
// (gt-cz07b). The score is inside the margin, so only the finding's severity
// keeps the rerun from happening: at the 0.70 a major usually scores, the
// margin alone would stop it and this test would pass with the severity rule
// gone.
func TestLandDoesNotRescoreARejectionWithAMajor(t *testing.T) {
	t.Parallel()
	f := barFixture(t, "0.8")
	rev := &sequenceReviewer{t: t, steps: []reviewStep{
		{verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.78, Findings: []Finding{
			{ID: "f-1", Severity: "major", Path: "a.go", Line: 4, Title: "the write is not atomic"}}}},
	}}
	l := f.lander()
	l.Reviewer = rev

	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectReview, LabelRework)
	if len(rev.inputs) != 1 {
		t.Fatalf("om ran %d time(s), want 1: a major finding is never re-scored", len(rev.inputs))
	}
	if got := parseVerdictComment(t, commentTexts(f.beadComments())[0]); got.rescore != "" {
		t.Errorf("verdict comment carries a re-score clause (%q) for a major rejection", got.rescore)
	}
}

// TestLandKeepsTheFirstVerdictWhenTheRerunErrors: the rerun is a second opinion,
// and one that reaches none leaves the review that decided the landing where it
// was — as does one that answers with something that is not a verdict at all
// (gt-cz07b).
func TestLandKeepsTheFirstVerdictWhenTheRerunErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		step reviewStep
	}{
		{name: "the run fails", step: reviewStep{err: errors.New("om review did not run: exec: om: not found")}},
		{name: "the run answers with no verdict", step: reviewStep{verdict: Verdict{Score: 0.81}}},
		{name: "the run answers with a word that is not one of om's", step: reviewStep{verdict: Verdict{Verdict: "comment_only", Score: 0.95}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := barFixture(t, "0.8")
			rev := &sequenceReviewer{t: t, steps: []reviewStep{
				{verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.78, Findings: []Finding{
					{ID: "f-1", Severity: "minor", Path: "a.go", Line: 4, Title: "the name is vague"}}}},
				tc.step,
			}}
			l := f.lander()
			l.Reviewer = rev
			var log strings.Builder
			l.Out = &log

			_, err := l.Land(context.Background(), f.work)
			f.assertRejected(t, err, RejectReview, LabelRework)
			if len(rev.inputs) != 2 {
				t.Fatalf("om ran %d time(s), want 2", len(rev.inputs))
			}
			last, ok := ParseRejectionNote(f.bead().Notes)
			if !ok || last.Receipt == nil || last.Receipt.Score != 0.78 {
				t.Errorf("receipt = %+v, want the first run's score 0.78", last.Receipt)
			}
			got := parseVerdictComment(t, commentTexts(f.beadComments())[0])
			if got.word != VerdictRequestChanges || got.score != "0.78" || got.rescore != "" {
				t.Errorf("verdict comment = %q, want the first run's request for changes at 0.78 with no re-score", commentTexts(f.beadComments())[0])
			}
			if !strings.Contains(log.String(), "0.78") {
				t.Errorf("the landing log does not name the score that stood:\n%s", log.String())
			}
		})
	}
}
