package land

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestVerdictReuseSuffixForm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cost time.Duration
		age  time.Duration
		want string
	}{
		{"minutes and seconds", 2*time.Minute + 31*time.Second, 14 * time.Minute, ", om reused (2m31s, 14m0s old)"},
		{"zero durations", 0, 0, ", om reused (0s, 0s old)"},
		{"negative age keeps its sign", time.Second, -5 * time.Minute, ", om reused (1s, -5m0s old)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := omReusedSuffix(tc.cost, tc.age); got != tc.want {
				t.Fatalf("omReusedSuffix = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVerdictReuseSuffixFormStageLine(t *testing.T) {
	t.Parallel()
	noLoad := func() (float64, bool) { return 0, false }
	got := stageLine([]string{"gate 3s"}, 0, false, noLoad) + omReusedSuffix(2*time.Minute+31*time.Second, 14*time.Minute)
	want := "stages: gate 3s, om reused (2m31s, 14m0s old)"
	if got != want {
		t.Fatalf("stage line = %q, want %q", got, want)
	}
}

func TestVerdictReuseApplyBar(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      Verdict
		bar     float64
		want    string
		wantSum string
	}{
		{"approve above the bar stays", Verdict{Verdict: VerdictApprove, Score: 0.9, Summary: "fine"}, 0.8, VerdictApprove, "fine"},
		{"approve at the bar stays", Verdict{Verdict: VerdictApprove, Score: 0.8, Summary: "fine"}, 0.8, VerdictApprove, "fine"},
		{"approve below the bar is request_changes", Verdict{Verdict: VerdictApprove, Score: 0.5, Summary: "fine"}, 0.8, VerdictRequestChanges, "score 0.50 is below the rig's .om.json threshold 0.80. fine"},
		{"request_changes is unchanged", Verdict{Verdict: VerdictRequestChanges, Score: 0.2, Summary: "no"}, 0.8, VerdictRequestChanges, "no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := applyBar(tc.in, tc.bar)
			if got.Verdict != tc.want || got.Summary != tc.wantSum {
				t.Fatalf("applyBar = %q %q, want %q %q", got.Verdict, got.Summary, tc.want, tc.wantSum)
			}
		})
	}
}

// TestVerdictReuseApplyBarMatchesReviewOnce is the drift pin: the same approve
// below the bar gives the same verdict and summary through reviewOnce, which
// applies the bar to a fresh om run, and through applyBar, which a reused
// verdict takes.
func TestVerdictReuseApplyBarMatchesReviewOnce(t *testing.T) {
	t.Parallel()
	var argv []string
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = fakeOM(t, `{"score":0.5,"verdict":"approve","summary":"looks fine"}`, 0, nil, &argv)
	inline, err := reviewAt(context.Background(), r, `{"threshold": 0.8}`, "/wt", "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if inline.Verdict != VerdictRequestChanges {
		t.Fatalf("reviewOnce verdict = %q, want %q: the scenario is not below the bar", inline.Verdict, VerdictRequestChanges)
	}
	reused := applyBar(Verdict{Verdict: VerdictApprove, Score: 0.5, Summary: "looks fine"}, 0.8)
	if reused.Verdict != inline.Verdict || reused.Summary != inline.Summary {
		t.Fatalf("applyBar = %q %q, reviewOnce = %q %q", reused.Verdict, reused.Summary, inline.Verdict, inline.Summary)
	}
}

// Reused-verdict fixture: the merged head the landing records differs from the
// head the stored review ran at, so a comment that names the wrong head fails.
const (
	reusedMergedHead   = "feedface12345678"
	reusedReviewedHead = "1111222233334444"
	reusedBase         = "abcdef0123456789"
)

// reusedRecord is a stored review of reusedReviewedHead on reusedBase, made 14
// minutes before now.
func reusedRecord(now time.Time) AheadRecord {
	return AheadRecord{Bead: "gt-eyzgl6", Head: reusedReviewedHead, Base: reusedBase, ReviewedAt: now.Add(-14 * time.Minute)}
}

// TestVerdictReuseCommentKeepsHeadMarker: the comment a reused verdict leaves is
// recognized by the verdict-comment check, so a second call writes nothing
// (gt-eyzgl6).
func TestVerdictReuseCommentKeepsHeadMarker(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l, w := f.lander(), f.work
	beads := &commentBeads{Fake: f.bd}
	l.Beads = beads
	w.CandidateHead = reusedMergedHead
	v := Verdict{Verdict: VerdictApprove, Score: 0.92}
	rec := reusedRecord(l.now())
	l.commentReused(w, v, rec)
	l.commentReused(w, v, rec)
	comments := f.beadComments()
	if len(comments) != 1 {
		t.Fatalf("%d comments after two reuses of one verdict; want one", len(comments))
	}
	if !verdictCommented(comments, verdictCommentHead(w.CandidateHead), "om approve ") {
		t.Errorf("comment %q is not recognized by the verdict-comment check", comments[0].Text)
	}
	if beads.calls != 1 {
		t.Errorf("AddComment calls = %d; want one write", beads.calls)
	}
}

// TestVerdictReuseCommentNamesBaseAndAge: the comment names the verdict and its
// score, the landing's merged head, the reviewed base and how long ago the
// verdict was stored, in the stage line's duration form. A stored verdict dated
// after the landing's clock keeps its sign (gt-eyzgl6).
func TestVerdictReuseCommentNamesBaseAndAge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		v              Verdict
		reviewedOffset time.Duration // ReviewedAt, from the landing's clock
		want           string
	}{
		{"an approve, 14 minutes old", Verdict{Verdict: VerdictApprove, Score: 0.92}, -14 * time.Minute,
			"om approve 0.92 [head feedface] (reused from abcdef01, 14m0s old)"},
		{"a request for changes names its own word", Verdict{Verdict: VerdictRequestChanges, Score: 0.4}, -14 * time.Minute,
			"om request_changes 0.40 [head feedface] (reused from abcdef01, 14m0s old)"},
		{"a review stored after the landing's clock keeps its sign", Verdict{Verdict: VerdictApprove, Score: 0.92}, 5 * time.Minute,
			"om approve 0.92 [head feedface] (reused from abcdef01, -5m0s old)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l, w := f.lander(), f.work
			l.Beads = &commentBeads{Fake: f.bd}
			w.CandidateHead = reusedMergedHead
			rec := reusedRecord(l.now())
			rec.ReviewedAt = l.now().Add(tc.reviewedOffset)
			l.commentReused(w, tc.v, rec)
			comments := f.beadComments()
			if len(comments) != 1 || comments[0].Text != tc.want {
				t.Fatalf("comments = %+v; want the one comment %q", comments, tc.want)
			}
		})
	}
}

// TestVerdictReuseCommentRefusesIncompleteInput: a reused verdict with no merged
// head, no reviewed base or no review time would name something that is not
// there, so it writes nothing and logs the refusal (gt-eyzgl6).
func TestVerdictReuseCommentRefusesIncompleteInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		head   string
		mutate func(*AheadRecord)
	}{
		{"no merged head", "", func(*AheadRecord) {}},
		{"no reviewed base", reusedMergedHead, func(r *AheadRecord) { r.Base = "" }},
		{"no review time", reusedMergedHead, func(r *AheadRecord) { r.ReviewedAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l, w := f.lander(), f.work
			beads := &commentBeads{Fake: f.bd}
			l.Beads = beads
			var log strings.Builder
			l.Out = &log
			w.CandidateHead = tc.head
			rec := reusedRecord(l.now())
			tc.mutate(&rec)
			l.commentReused(w, Verdict{Verdict: VerdictApprove, Score: 0.92}, rec)
			if beads.calls != 0 || len(f.beadComments()) != 0 {
				t.Errorf("AddComment calls = %d, comments = %d; want the refusal to write nothing", beads.calls, len(f.beadComments()))
			}
			if !strings.Contains(log.String(), "is not commented") {
				t.Errorf("log = %q; want the refusal logged", log.String())
			}
		})
	}
}

// TestVerdictReuseCommentQueuesAFailedWrite: a reused verdict's comment is tried
// once; one whose write fails is logged and queued under the merged head's
// marker, holding the reused text, for a later pass (gt-eyzgl6).
func TestVerdictReuseCommentQueuesAFailedWrite(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l, w := f.lander(), f.work
	beads := &commentBeads{Fake: f.bd, flaky: 1}
	l.Beads = beads
	q := queueAt(t, false)
	l.Records = &q
	var log strings.Builder
	l.Out = &log
	w.CandidateHead = reusedMergedHead
	l.commentReused(w, Verdict{Verdict: VerdictApprove, Score: 0.92}, reusedRecord(l.now()))
	if beads.calls != 1 {
		t.Errorf("AddComment calls = %d; want one try", beads.calls)
	}
	if !strings.Contains(log.String(), "could not comment the reused om verdict") {
		t.Errorf("log = %q; want the failed write named", log.String())
	}
	got, err := q.Pending()
	key := verdictCommentHead(w.CandidateHead)
	if err != nil || len(got) != 1 || got[0].Kind != RecordKindComment || got[0].Key != key {
		t.Fatalf("Pending = %+v, %v; want one comment keyed by %q", got, err, key)
	}
	if want := "om approve 0.92 [head feedface] (reused from abcdef01, 14m0s old)"; got[0].Text != want {
		t.Errorf("queued text = %q; want %q", got[0].Text, want)
	}
}
