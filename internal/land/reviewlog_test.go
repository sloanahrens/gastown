package land

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// The reviews log is the file the dashboard's rework table reads: one line per
// om verdict that judged a tree, appended by the landing worker (gt-mctxx).

func reviewsFileIn(t *testing.T) *ReviewsFile {
	t.Helper()
	return &ReviewsFile{Path: filepath.Join(t.TempDir(), "landings", ReviewsFileName)}
}

func TestReviewsFileAppendAndSince(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	if recs, err := f.Since(day); err != nil || recs != nil {
		t.Fatalf("Since on a log that does not exist = %v, %v; want none and no error", recs, err)
	}
	old := ReviewRecord{BeadID: "gt-old", Rig: "gastown", Attempt: 1, Score: 0.5, Verdict: VerdictRequestChanges, Findings: 2, Bar: 0.8, At: day.Add(-time.Hour)}
	cur := ReviewRecord{BeadID: "gt-new", Rig: "gastown", Attempt: 2, Score: 0.91, Verdict: VerdictApprove, Findings: 0, Bar: 0.8, At: day}
	for _, rec := range []ReviewRecord{old, cur} {
		if err := f.Append(rec); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	recs, err := f.Since(day)
	if err != nil || len(recs) != 1 || recs[0] != cur {
		t.Fatalf("Since(%v) = %+v, %v; want only %+v", day, recs, err, cur)
	}
	if all, _ := f.Since(day.Add(-24 * time.Hour)); len(all) != 2 || all[0] != old {
		t.Errorf("Since(a day earlier) = %+v, want both records in file order", all)
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"bead":"gt-new"`, `"attempt":2`, `"score":0.91`, `"verdict":"approve"`, `"findings":0`, `"bar":0.8`, `"at":"2026-10-14T00:00:00Z"`, `"rig":"gastown"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the log line lacks %s:\n%s", want, raw)
		}
	}
}

// A line left torn by a crash is repaired before the next record, and a line
// that does not parse is skipped, so the records around it still read.
func TestReviewsFileSurvivesATornLine(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	if err := f.Append(ReviewRecord{BeadID: "gt-a", Verdict: VerdictApprove, At: at}); err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(f.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(`{"bead":"gt-torn","ver`); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
	if err := f.Append(ReviewRecord{BeadID: "gt-b", Verdict: VerdictRequestChanges, At: at}); err != nil {
		t.Fatal(err)
	}
	recs, err := f.Since(time.Time{})
	if err != nil || len(recs) != 2 || recs[0].BeadID != "gt-a" || recs[1].BeadID != "gt-b" {
		t.Fatalf("Since = %+v, %v; want gt-a and gt-b around the torn line", recs, err)
	}
}

// A rig named reviews would share a file with the log, so no landings file is
// made for it.
func TestRigLandingsFileRefusesTheReviewsLogName(t *testing.T) {
	t.Parallel()
	if _, err := RigLandingsFile(t.TempDir(), "reviews"); err == nil {
		t.Fatal("RigLandingsFile accepted the rig name that is the reviews log's file")
	}
	if got := TownReviewsFile("/town").Path; got != filepath.Join("/town", ".runtime", "landings", ReviewsFileName) {
		t.Errorf("TownReviewsFile path = %s", got)
	}
}

// Every om decision the landing reaches is one line: a rejection, then the
// approval after the rework, numbered as the bead's rejection blocks are, under
// the bar the reviewed tree carried.
func TestLandLogsEachOMVerdict(t *testing.T) {
	t.Parallel()
	f := barFixture(t, "0.8")
	reviews := reviewsFileIn(t)
	l := f.lander()
	l.Reviews = reviews
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictRequestChanges, Score: 0.3, Findings: []Finding{{ID: "f-1", Severity: "major", Path: "b.txt", Line: 1, Title: "wrong"}}}, nil
	}
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectReview, LabelRework)

	f.requeue(1)
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictApprove, Score: 0.92, Findings: []Finding{{ID: "f-2", Severity: "minor", Path: "b.txt", Line: 1, Title: "name"}}}, nil
	}
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("second Land: %v", err)
	}

	recs, err := reviews.Since(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	want := []ReviewRecord{
		{BeadID: f.work.BeadID, Rig: f.work.Rig, Attempt: 1, Score: 0.3, Verdict: VerdictRequestChanges, Findings: 1, Bar: 0.8, At: at},
		{BeadID: f.work.BeadID, Rig: f.work.Rig, Attempt: 2, Score: 0.92, Verdict: VerdictApprove, Findings: 1, Bar: 0.8, At: at},
	}
	if len(recs) != len(want) || recs[0] != want[0] || recs[1] != want[1] {
		t.Fatalf("review log = %+v, want %+v", recs, want)
	}
}

// A review that judged nothing is not a verdict to count, and a log that cannot
// be written never holds a landing.
func TestLandLogsNothingForAReviewThatJudgedNothingAndSurvivesALogFailure(t *testing.T) {
	t.Parallel()

	skipped := newLandFixture(t)
	skipLog := reviewsFileIn(t)
	l := skipped.lander()
	l.Reviews = skipLog
	skipped.review.fn = func(string) (Verdict, error) { return Verdict{Verdict: VerdictSkipped}, nil }
	if _, err := l.Land(context.Background(), skipped.work); err != nil {
		t.Fatalf("Land with a skipped review: %v", err)
	}
	if recs, _ := skipLog.Since(time.Time{}); len(recs) != 0 {
		t.Errorf("a skipped review logged %+v, want nothing", recs)
	}

	broken := newLandFixture(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l = broken.lander()
	l.Reviews = &ReviewsFile{Path: filepath.Join(blocker, ReviewsFileName)}
	var out strings.Builder
	l.Out = &out
	broken.review.fn = func(string) (Verdict, error) { return Verdict{Verdict: VerdictApprove, Score: 0.9}, nil }
	if _, err := l.Land(context.Background(), broken.work); err != nil {
		t.Fatalf("Land with an unwritable reviews log: %v", err)
	}
	if !strings.Contains(out.String(), "could not append to the reviews log") {
		t.Errorf("the failed append is not in the landing log:\n%s", out.String())
	}
	appendErr := l.Reviews.Append(ReviewRecord{})
	if appendErr == nil {
		t.Fatal("the reviews log under a plain file accepted an append")
	}
	comments, err := broken.bd.Comments(broken.work.BeadID)
	if err != nil {
		t.Fatal(err)
	}
	want := "om approve 0.90 (attempt 1) is not in the reviews log: " + elideMiddle(NoteField(appendErr.Error()), reviewErrorReasonMax)
	var onBead []string
	for _, c := range comments {
		if strings.Contains(c.Text, "is not in the reviews log") {
			onBead = append(onBead, c.Text)
		}
	}
	if len(onBead) != 1 || onBead[0] != want {
		t.Errorf("failed-append comments on the bead = %q, want exactly [%q]", onBead, want)
	}
	if !strings.Contains(want, "not a directory") {
		t.Errorf("the comment does not carry the append's reason: %q", want)
	}
}

// An approval whose findings the landing refuses is a rejection, and the line
// says so: om's own verdict stays approve, and Rejected is what the rework
// table counts (gt-vu18r). An approval the findings let through is not marked.
func TestLogReviewRecordsFindingsRejection(t *testing.T) {
	t.Parallel()
	f := barFixture(t, "0.8")
	reviews := reviewsFileIn(t)
	l := f.lander()
	l.Reviews = reviews
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictApprove, Score: 0.93, Findings: []Finding{{ID: "f-1", Severity: "major", Path: "b.txt", Line: 1, Title: "not atomic"}}}, nil
	}
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectReview, LabelRework)

	f.requeue(1)
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictApprove, Score: 0.93, Findings: []Finding{{ID: "f-2", Severity: "minor", Path: "b.txt", Line: 1, Title: "name"}}}, nil
	}
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("second Land: %v", err)
	}

	recs, err := reviews.Since(time.Time{})
	if err != nil || len(recs) != 2 {
		t.Fatalf("review log = %+v, %v; want the bounced approval and the landed one", recs, err)
	}
	if recs[0].Verdict != VerdictApprove || !recs[0].Rejected || recs[0].Attempt != 1 {
		t.Errorf("bounced approval logged as %+v, want attempt 1, om's approve and Rejected", recs[0])
	}
	if recs[1].Rejected || recs[1].Attempt != 2 {
		t.Errorf("landed approval logged as %+v, want attempt 2 and not Rejected", recs[1])
	}
}

// seedReviewRejections appends n review rejection blocks of earlier heads to
// the fixture's bead, so the landing that follows is round n+1: past the
// ceiling once n reaches MaxReworkAttempts. The heads differ from the work's,
// so the blocks are history and not an unfinished pass (gt-zqqcr).
func seedReviewRejections(t *testing.T, f *landFixture, n int) {
	t.Helper()
	for attempt := 1; attempt <= n; attempt++ {
		block := FormatRejectionNote(RejectionNote{
			Attempt: attempt, Kind: string(RejectReview), Reason: "earlier refusal",
			Branch: f.work.Branch, Target: f.work.Target, MR: f.work.BeadID,
			Head: fmt.Sprintf("earlier%d", attempt),
		})
		if err := f.bd.Update(f.work.BeadID, beads.UpdateOptions{AppendNotes: block}); err != nil {
			t.Fatalf("seeding rejection %d: %v", attempt, err)
		}
	}
}

// TestLogReviewAttemptMatchesTheBlockAtTheCeiling is gt-k10bq: a refused round
// above MaxReworkAttempts is recorded under the attempt its MERGE REJECTION
// block carries. The block's header stops at the ceiling (gt-ern0y), so a line
// that kept counting named an attempt no block has, and the rework pass — which
// matches a refusal to its block by number (internal/jev/rework) — read the
// refusal as unreadable and skipped it. Both shapes of refusal count: om's own
// request_changes and an approval its findings turn away.
func TestLogReviewAttemptMatchesTheBlockAtTheCeiling(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		verdict Verdict
	}{
		{"request_changes", Verdict{Verdict: VerdictRequestChanges, Score: 0.4}},
		{"an approval its findings bounce", Verdict{Verdict: VerdictApprove, Score: 0.93,
			Findings: []Finding{{ID: "f-1", Severity: "major", Path: "b.txt", Line: 1, Title: "not atomic"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := barFixture(t, "0.8")
			seedReviewRejections(t, f, MaxReworkAttempts)
			reviews := reviewsFileIn(t)
			l := f.lander()
			l.Reviews = reviews
			verdict := tc.verdict
			f.review.fn = func(string) (Verdict, error) { return verdict, nil }

			_, err := l.Land(context.Background(), f.work)
			var rej *Rejection
			if !errors.As(err, &rej) || rej.Kind != RejectReview || rej.RecordErr != nil {
				t.Fatalf("Land = %T %v, want a recorded review rejection", err, err)
			}
			if !beads.HasLabel(f.bead(), LabelNeedsHuman) {
				t.Errorf("bead labels = %v, want the capped round to go to a human", f.bead().Labels)
			}
			block, ok := ParseRejectionNote(f.bead().Notes)
			if !ok || block.Attempt != MaxReworkAttempts {
				t.Fatalf("the round's block reads attempt %d (%v), want the ceiling %d:\n%s",
					block.Attempt, ok, MaxReworkAttempts, f.bead().Notes)
			}
			recs, err := reviews.Since(time.Time{})
			if err != nil || len(recs) != 1 {
				t.Fatalf("review log = %+v, %v; want the one verdict this landing reached", recs, err)
			}
			if recs[0].Attempt != block.Attempt {
				t.Errorf("reviews log names attempt %d for a block that says %d, so a reader cannot match the refusal to its block",
					recs[0].Attempt, block.Attempt)
			}
			if !recs[0].IsRejection() {
				t.Errorf("the line records %+v, want a refusal", recs[0])
			}
		})
	}
}

// A line written before the Rejected field parses as the verdict alone, and a
// record that is not rejected writes no field.
func TestReviewRecordRejectedIsOptionalOnTheLine(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"bead":"gt-old","rig":"gastown","attempt":1,"score":0.9,"verdict":"approve","findings":0,"bar":0.8,"at":"2026-10-14T00:00:00Z"}` + "\n"
	if err := os.WriteFile(f.Path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := f.Since(time.Time{})
	if err != nil || len(recs) != 1 || recs[0].Rejected || recs[0].Verdict != VerdictApprove {
		t.Fatalf("old line read as %+v, %v; want an approval that is not Rejected", recs, err)
	}
	if err := f.Append(ReviewRecord{BeadID: "gt-new", Verdict: VerdictApprove, At: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f.Path)
	if strings.Contains(string(raw), `"rejected"`) {
		t.Errorf("a record that is not rejected wrote the field:\n%s", raw)
	}
}
