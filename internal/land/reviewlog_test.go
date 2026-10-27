package land

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/authorkind"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/promptstamp"
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
	if err != nil || len(recs) != 1 || !reflect.DeepEqual(recs[0], cur) {
		t.Fatalf("Since(%v) = %+v, %v; want only %+v", day, recs, err, cur)
	}
	if all, _ := f.Since(day.Add(-24 * time.Hour)); len(all) != 2 || !reflect.DeepEqual(all[0], old) {
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

// TestReviewsFileQueuedSkipsAnOverlongLine: a line past the reader's bound is
// skipped, not a failed read. A read that failed would fail the same way on
// every later read, so one bad line would stop the queue for its rig for good;
// the lines around it are still the queue (gt-3arxj).
func TestReviewsFileQueuedSkipsAnOverlongLine(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"bead":"gt-q","followup_queue":{"findings":[{"claim":"` + strings.Repeat("x", 2<<20) + `"}]}}`
	if err := os.WriteFile(f.Path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.AppendQueue(queueLine("gt-after", "gastown", queuedFinding)); err != nil {
		t.Fatal(err)
	}
	queued, err := f.Queued()
	if err != nil {
		t.Fatalf("Queued on an overlong line = %v, want the line skipped", err)
	}
	if len(queued) != 1 || queued[0].Bead != "gt-after" {
		t.Fatalf("Queued = %+v, want the line after the overlong one", queued)
	}
}

// TestAppendQueueKeepsEveryLineReadable: a landing's held findings are split
// across as many queue lines as the reader's bound needs, each claim bounded,
// so the feed can always read back what it queued (gt-3arxj).
func TestAppendQueueKeepsEveryLineReadable(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	var findings []ReviewFindingText
	for i := 0; i < 3*maxQueueLineFindings; i++ {
		findings = append(findings, ReviewFindingText{File: "a.go", Title: "name it", Claim: strings.Repeat("c", 3*maxRecordedClaimBytes)})
	}
	if err := f.AppendQueue(queueLine("gt-many", "gastown", findings...)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("the queue was written as %d line(s), want the findings split", len(lines))
	}
	for i, line := range lines {
		if len(line) > maxReviewLineBytes {
			t.Errorf("line %d is %d bytes, past the reader's %d-byte bound", i, len(line), maxReviewLineBytes)
		}
	}
	queued, err := f.Queued()
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, q := range queued {
		for _, ft := range q.Findings {
			got++
			if len(ft.Claim) > maxRecordedClaimBytes+len(truncatedClaim) {
				t.Errorf("a claim is %d bytes, past the recorded bound", len(ft.Claim))
			}
		}
	}
	if got != len(findings) {
		t.Errorf("read back %d findings, want all %d", got, len(findings))
	}
}

// TestCompactKeepsEveryLineItDoesNotDrop: compaction drops the queue lines the
// feed is done with and copies every other line byte for byte, so a verdict
// written before this change — a count-only record — reads as it did after the
// log is compacted (gt-3arxj).
func TestCompactKeepsEveryLineItDoesNotDrop(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	countOnly := `{"bead":"gt-old","rig":"gastown","attempt":1,"score":0.8,"verdict":"approve","findings":4,"bar":0.7,"at":"2026-10-01T00:00:00Z"}`
	done, err := json.Marshal(ReviewRecord{BeadID: "gt-done", Rig: "gastown", At: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		Queue: &FollowupQueue{Landed: queuedLanded, Score: 0.7, Findings: []ReviewFindingText{{Title: "done"}}}})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := json.Marshal(ReviewRecord{BeadID: "gt-wait", Rig: "gastown", At: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		Queue: &FollowupQueue{Landed: queuedLanded, Score: 0.7, Findings: []ReviewFindingText{{Title: "wait"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path, []byte(countOnly+"\n"+string(done)+"\n"+string(waiting)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dropped, err := f.Compact(func(q QueuedFollowup) bool { return q.Bead == "gt-done" })
	if err != nil || dropped != 1 {
		t.Fatalf("Compact = %d dropped, %v; want the one consumed line", dropped, err)
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if want := countOnly + "\n" + string(waiting) + "\n"; string(raw) != want {
		t.Errorf("compacted log =\n%s\nwant\n%s", raw, want)
	}
}

// TestCompactLeavesTheLogAloneWithNothingConsumed: a feed with no consumed
// queue line writes nothing, so a log with a line too long to compact is still
// a log nothing rewrote (gt-3arxj).
func TestCompactLeavesTheLogAloneWithNothingConsumed(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	if err := f.AppendQueue(queueLine("gt-wait", "gastown", queuedFinding)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	dropped, err := f.Compact(func(QueuedFollowup) bool { return false })
	if err != nil || dropped != 0 {
		t.Fatalf("Compact = %d dropped, %v; want nothing dropped", dropped, err)
	}
	after, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("an unconsumed queue line was rewritten:\n%s", after)
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
		return Verdict{Verdict: VerdictApprove, Score: 0.92, Findings: []Finding{{ID: "f-2", Severity: "minor", Path: "b.txt", Line: 1, Title: "name", Detail: "the error names nothing"}}}, nil
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
		{BeadID: f.work.BeadID, Rig: f.work.Rig, Attempt: 1, Score: 0.3, Verdict: VerdictRequestChanges, Findings: 1, Bar: 0.8, At: at,
			Branch: f.work.Branch, AuthorKind: authorkind.Polecat,
			FindingText: []ReviewFindingText{{ID: "f-1", Severity: "major", File: "b.txt", Line: 1, Title: "wrong"}}},
		{BeadID: f.work.BeadID, Rig: f.work.Rig, Attempt: 2, Score: 0.92, Verdict: VerdictApprove, Findings: 1, Bar: 0.8, At: at,
			Branch: f.work.Branch, AuthorKind: authorkind.Polecat,
			FindingText: []ReviewFindingText{{ID: "f-2", Severity: "minor", File: "b.txt", Line: 1, Title: "name", Claim: "the error names nothing"}}},
	}
	if len(recs) != len(want) || !reflect.DeepEqual(recs, want) {
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

// cutBranch makes branch on origin carry the fixture's change and points the
// fixture's work and its ready note at it, so a review's author branch is not
// the fixture's polecat branch.
func (f *landFixture) cutBranch(branch string) {
	f.t.Helper()
	f.git.SetRef(f.t, f.origin, "refs/heads/"+branch, f.base)
	head := f.git.Commit(f.t, f.origin, branch, "feat: add b", map[string]string{"b.txt": "work\n"})
	f.work.Branch, f.work.Head = branch, head
	f.bd.Seed(beads.Issue{ID: f.work.BeadID, Title: "add b", Status: "hooked", Type: "task",
		Assignee: "gastown/polecats/opal", Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
}

// What the review log says about the author is on its line: the branch the
// work came in on and the kind of seat that wrote it, so the per-kind view
// reads no bead's notes (gt-b9j3o.16).
func TestLandRecordsTheAuthorsBranchAndKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, branch, wantKind string }{
		{"a polecat's branch", "polecat/opal/gt-abc+x1", authorkind.Polecat},
		{"a crew member's branch", "crew/merlin/gt-abc", authorkind.Crew},
		{"the overseer's own branch", "crew/overseer/gt-abc", authorkind.Overseer},
		{"a branch the town does not name by prefix", "merlin/gt-abc", authorkind.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := barFixture(t, "0.8")
			f.cutBranch(tc.branch)
			reviews := reviewsFileIn(t)
			l := f.lander()
			l.Reviews = reviews
			if _, err := l.Land(context.Background(), f.work); err != nil {
				t.Fatalf("Land: %v", err)
			}
			recs, err := reviews.Since(time.Time{})
			if err != nil || len(recs) != 1 {
				t.Fatalf("review log = %+v, %v; want the one verdict", recs, err)
			}
			if recs[0].Branch != tc.branch || recs[0].AuthorKind != tc.wantKind {
				t.Errorf("record carries branch %q and kind %q, want %q and %q",
					recs[0].Branch, recs[0].AuthorKind, tc.branch, tc.wantKind)
			}
			raw, err := os.ReadFile(reviews.Path)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{`"branch":"` + tc.branch + `"`, `"author_kind":"` + tc.wantKind + `"`} {
				if !strings.Contains(string(raw), want) {
					t.Errorf("the log line lacks %s:\n%s", want, raw)
				}
			}
		})
	}
}

// The prompt the author seat ran is on the review's line when the spawn
// stamped it on the work bead, and left out entirely when it did not: an empty
// prompt_sha would read as a prompt nobody stored (gt-b9j3o.16). The stamps
// are built with the spawn's own rule (promptstamp) and posted in the order a
// bead's store keeps them, oldest first, so the last one a rework respawn
// posted is the one the record names.
func TestLandRecordsThePromptStampWhenTheBeadCarriesOne(t *testing.T) {
	t.Parallel()
	const stored = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	stamp := promptstamp.Format(stored, 4096)
	seat := promptstamp.Author("gastown", "opal")
	for _, tc := range []struct {
		name, author string
		comments     []string
		want         string
	}{
		{"the author seat's stamp", seat, []string{stamp}, stored[:promptstamp.HashLen]},
		{"the newest stamp, after a rework respawn stamped again", seat,
			[]string{stamp, promptstamp.Format("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 4200)}, "ffffffffffff"},
		{"no stamp on the bead", "", nil, ""},
		{"a stamp another seat wrote", promptstamp.Author("gastown", "corpus"), []string{stamp}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := barFixture(t, "0.8")
			for _, comment := range tc.comments {
				if err := f.bd.AddCommentAs(f.work.BeadID, tc.author, comment); err != nil {
					t.Fatal(err)
				}
			}
			reviews := reviewsFileIn(t)
			l := f.lander()
			l.Reviews = reviews
			if _, err := l.Land(context.Background(), f.work); err != nil {
				t.Fatalf("Land: %v", err)
			}
			recs, err := reviews.Since(time.Time{})
			if err != nil || len(recs) != 1 {
				t.Fatalf("review log = %+v, %v; want the one verdict", recs, err)
			}
			if recs[0].PromptSHA != tc.want {
				t.Errorf("record carries prompt_sha %q, want %q", recs[0].PromptSHA, tc.want)
			}
			raw, err := os.ReadFile(reviews.Path)
			if err != nil {
				t.Fatal(err)
			}
			has := strings.Contains(string(raw), `"prompt_sha"`)
			if has != (tc.want != "") {
				t.Errorf("the log line carries prompt_sha = %v, want %v:\n%s", has, tc.want != "", raw)
			}
		})
	}
}

// A bead whose comments cannot be read is reviewed without the prompt stamp:
// the record carries no prompt_sha rather than failing the landing, and the
// log names the read that failed so the empty field is not read as a bead with
// no stamp (gt-b9j3o.16).
func TestLandRecordsNoPromptWhenTheBeadsCommentsCannotBeRead(t *testing.T) {
	t.Parallel()
	f := barFixture(t, "0.8")
	if err := f.bd.AddCommentAs(f.work.BeadID, promptstamp.Author("gastown", "opal"),
		promptstamp.Format("0123456789abcdef", 4096)); err != nil {
		t.Fatal(err)
	}
	reviews := reviewsFileIn(t)
	var logged strings.Builder
	l := f.lander()
	l.Reviews = reviews
	l.Out = &logged
	l.Beads = &failingBeads{Fake: f.bd, failComments: true}
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	recs, err := reviews.Since(time.Time{})
	if err != nil || len(recs) != 1 {
		t.Fatalf("review log = %+v, %v; want the one verdict", recs, err)
	}
	if recs[0].PromptSHA != "" {
		t.Errorf("record carries prompt_sha %q, want none when the comments could not be read", recs[0].PromptSHA)
	}
	// The work-item read's own line, not the accepted-risk read's, which fails
	// on the same bead: dropping either would hide which half lost its input.
	if s := logged.String(); !strings.Contains(s, "om reads the brief without them, and the review record carries no prompt") {
		t.Errorf("the landing log does not name the failed work-item read:\n%s", s)
	}
}

// A record line written before branch, author_kind and prompt_sha parses as
// the record it was: the fields stay empty rather than making the line
// unreadable or its author unknown-shaped (gt-b9j3o.16).
func TestReviewRecordReadsALineFromBeforeTheAuthorFields(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"bead":"gt-old","rig":"gastown","attempt":2,"score":0.7,"verdict":"request_changes","agent":"opus","findings":3,"bar":0.8,"at":"2026-10-24T00:00:00Z"}` + "\n"
	if err := os.WriteFile(f.Path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := f.Since(time.Time{})
	if err != nil || len(recs) != 1 {
		t.Fatalf("Since = %+v, %v; want the old line read", recs, err)
	}
	got := recs[0]
	if got.Branch != "" || got.AuthorKind != "" || got.PromptSHA != "" {
		t.Errorf("the old line read as %+v, want no branch, author_kind or prompt_sha", got)
	}
	if got.Agent != "opus" || got.Attempt != 2 || got.Verdict != VerdictRequestChanges {
		t.Errorf("the old line's own fields read as %+v", got)
	}
}
