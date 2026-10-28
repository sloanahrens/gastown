package land

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// The follow-up queue: the findings a landing's backpressure bound held back
// are kept whole in the reviews log, and a later landing with room files them
// (gt-3arxj).

const (
	queuedLanded = "fedcba9876543210fedcba9876543210fedcba98"
	queuedClaim  = "the nil check panics on an empty map"
)

// queuedFinding is the finding every queue case is about: one minor with every
// field the record keeps.
var queuedFinding = ReviewFindingText{
	ID: "f-9", Severity: "minor", File: "internal/land/note.go", Line: 12,
	Title: "name the error", Claim: queuedClaim,
}

// queueLine is one held landing's line as the feed writes it.
func queueLine(bead, rig string, findings ...ReviewFindingText) QueuedFollowup {
	return QueuedFollowup{Bead: bead, Rig: rig, At: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		Landed: queuedLanded, Score: 0.7, Findings: findings}
}

// TestFollowupFeedQueuesHeldFindings: a finding the bound keeps from a bead is
// not dropped. Its file, line, title and claim go into the landing's review
// log, so the bead is filed later from the record alone (gt-3arxj).
func TestFollowupFeedQueuesHeldFindings(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{{
		ID: "f-1", Severity: "minor", Path: "internal/land/note.go", Line: 260,
		Title: "name the error", Detail: queuedClaim,
	}})
	f.fake.existing = openFollowupBeads(DefaultFollowupBackpressure)
	log := f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads at the backpressure bound", len(f.fake.created))
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("the log holds %d queue lines, want one:\n%s", len(queued), log)
	}
	if queued[0].At.IsZero() {
		t.Error("the queue line carries no time")
	}
	queued[0].At = time.Time{}
	want := queueLine(f.work.BeadID, f.work.Rig, ReviewFindingText{
		ID: "f-1", Severity: "minor", File: "internal/land/note.go", Line: 260,
		Title: "name the error", Claim: queuedClaim,
	})
	want.Landed, want.Score, want.At = f.result.LandedCommit, 0.8, time.Time{}
	if !reflect.DeepEqual(queued[0], want) {
		t.Errorf("queue line = %+v, want %+v", queued[0], want)
	}
	if !strings.Contains(log, "queued the 1 finding the bound held back") {
		t.Errorf("log = %q, want the queued findings named", log)
	}
	// The queue line is not a verdict, so the readers that count verdicts, and
	// join a bead to its first one, do not see it (gt-3arxj).
	if verdicts, err := f.reviews.Since(time.Time{}); err != nil || len(verdicts) != 0 {
		t.Errorf("Since = %+v, %v; want no verdicts, the queue line left out", verdicts, err)
	}
}

// TestFollowupFeedFilesQueuedFindingsOnceTheBoundHasRoom: the queue waits for
// the open count to fall, and the next landing with room files it as the held
// landing's own bead, naming that landing and its commit (gt-3arxj).
func TestFollowupFeedFilesQueuedFindingsOnceTheBoundHasRoom(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil) // nothing of its own to file
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
	f.fake.existing = openFollowupBeads(1)
	log := f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads from the queue, want one:\n%s", len(f.fake.created), log)
	}
	body := f.fake.created[0].Body
	for _, want := range []string{"gt-held", queuedLanded, "name the error"} {
		if !strings.Contains(body, want) {
			t.Errorf("bead body lacks %q:\n%s", want, body)
		}
	}
	comments, err := f.bd.Comments("gt-held")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || !strings.Contains(comments[0].Text, "gt-filed") {
		t.Errorf("comments on gt-held = %v, want the filed bead named", commentTexts(comments))
	}
}

// TestFollowupFeedDropsAConsumedQueueLine: once the feed has filed a queued
// landing's findings, the line that held them is dropped from the log, so a
// town does not rescan every queue line it ever wrote (gt-3arxj).
func TestFollowupFeedDropsAConsumedQueueLine(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil)
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
	f.fake.existing = openFollowupBeads(1)
	log := f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads from the queue, want one:\n%s", len(f.fake.created), log)
	}
	if !strings.Contains(log, "dropped 1 consumed queue line") {
		t.Errorf("log = %q, want the dropped line named", log)
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("queue = %+v, want the drained line gone", queued)
	}
}

// TestFollowupFeedKeepsAQueueLineItCouldNotFile: a line is consumed only when
// every finding on it is covered, so a bead that could not be filed leaves its
// line in the log for the next landing with room rather than dropping the
// finding with the line (gt-3arxj).
func TestFollowupFeedKeepsAQueueLineItCouldNotFile(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil)
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
	f.fake.existing = openFollowupBeads(1)
	f.fake.createErr = errors.New("bead store is down")
	log := f.run()
	if !strings.Contains(log, "could not file the follow-up bead") {
		t.Errorf("log = %q, want the failed file named", log)
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Bead != "gt-held" {
		t.Errorf("queue = %+v, want the unfiled line kept", queued)
	}
}

// TestFollowupFeedLeavesAQueuedFindingItsBeadAlreadyCovers: the queue is filed
// by fingerprint against every follow-up, open or closed, so a finding that was
// filed and has since been fixed is neither filed again nor commented on — the
// queue line is the feed's bookkeeping, not a finding another landing raised
// (gt-3arxj).
func TestFollowupFeedLeavesAQueuedFindingItsBeadAlreadyCovers(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil)
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.fake.existing = []ExistingFollowup{{
		ID: "gt-fixed", Title: "Found: name the error", Status: "closed",
		Labels: []string{FollowUpLabel, Fingerprint(queuedFinding.File, queuedFinding.Title)},
	}}
	f.bd.Seed(beads.Issue{ID: "gt-fixed", Title: "Found: name the error", Status: "closed", Type: "bug"})
	log := f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads for a finding a closed bead already covers:\n%s", len(f.fake.created), log)
	}
	comments, err := f.bd.Comments("gt-fixed")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 0 {
		t.Errorf("comments = %v, want none on a closed bead the queue repeats", commentTexts(comments))
	}
	// The line had nothing left to file, so the feed is done with it.
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("queue = %+v, want the covered line dropped", queued)
	}
}

// TestFollowupFeedLeavesAnotherRigsQueueAlone: the review log is the town's, so
// a rig's feed files its own queue and no other rig's (gt-3arxj).
func TestFollowupFeedLeavesAnotherRigsQueueAlone(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil)
	if err := f.reviews.AppendQueue(queueLine("gt-held", "longeye", queuedFinding)); err != nil {
		t.Fatal(err)
	}
	log := f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads from another rig's queue:\n%s", len(f.fake.created), log)
	}
}

// TestFollowupFeedCompactionKeepsAnotherRigsLine: the log is
// the town's, so compacting after this rig drained its own line must not drop
// another rig's line whose findings read like a bead this rig just filed: that
// rig has not filed it, and the finding would be lost (gt-3arxj).
func TestFollowupFeedCompactionKeepsAnotherRigsLine(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil)
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	if err := f.reviews.AppendQueue(queueLine("lo-held", "longeye", queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
	f.fake.existing = openFollowupBeads(1)
	log := f.run()
	if !strings.Contains(log, "dropped 1 consumed queue line") {
		t.Fatalf("log = %q, want this rig's drained line dropped", log)
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Rig != "longeye" || queued[0].Bead != "lo-held" {
		t.Errorf("queue = %+v, want only the other rig's line left", queued)
	}
}

// TestFollowupFeedKeepsAQueueLineUntilTheBoundHasRoom: a queue line the bound
// cannot file stays where it is. The feed appends no second line for it, so the
// queue cannot grow by a landing that filed nothing (gt-3arxj).
func TestFollowupFeedKeepsAQueueLineUntilTheBoundHasRoom(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil)
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.fake.existing = openFollowupBeads(DefaultFollowupBackpressure)
	f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads at the backpressure bound", len(f.fake.created))
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("the log holds %d queue lines, want the one it started with", len(queued))
	}
}

// TestFollowupFeedSurvivesAnUnwritableQueue: the held findings are queued where
// the log takes the line, and a log that cannot is a feed line rather than a
// landing failure: the feed is fail-soft like every other part of the landing's
// tail (gt-3arxj).
func TestFollowupFeedSurvivesAnUnwritableQueue(t *testing.T) {
	t.Parallel()
	t.Run("a reviews log that cannot be read or written", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.fake.existing = openFollowupBeads(DefaultFollowupBackpressure)
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.lander.Reviews = &ReviewsFile{Path: filepath.Join(blocker, ReviewsFileName)}
		log := f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads at the backpressure bound", len(f.fake.created))
		}
		if !strings.Contains(log, "could not read the om-followup queue") {
			t.Errorf("log = %q, want the unreadable queue named", log)
		}
		if !strings.Contains(log, "could not queue the 1 finding the bound held back") {
			t.Errorf("log = %q, want the unqueued findings named", log)
		}
	})
	t.Run("no reviews log at all", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.fake.existing = openFollowupBeads(DefaultFollowupBackpressure)
		f.lander.Reviews = nil
		log := f.run()
		if !strings.Contains(log, "no reviews log to queue the 1 finding the bound held back") {
			t.Errorf("log = %q, want the missing log named", log)
		}
	})
}

// TestFollowupFeedDrainsAQueueLineInParts: the bound can leave room for some of
// a queue line's findings and not the rest. The line then stays with the
// findings no bead covers yet — the compaction a consumed line beside it
// triggers must not take it — and a later landing with room files them, so the
// line is consumed only once nothing on it is left to file (gt-3arxj).
func TestFollowupFeedDrainsAQueueLineInParts(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil)
	first := ReviewFindingText{ID: "f-1", Severity: "minor", File: "internal/land/note.go", Line: 12,
		Title: "name the error", Claim: queuedClaim}
	second := ReviewFindingText{ID: "f-2", Severity: "minor", File: "internal/metrics/panel.go", Line: 40,
		Title: "count the row", Claim: "the row is skipped"}
	// A line the bound can file whole, so the compaction runs in the same pass.
	if err := f.reviews.AppendQueue(queueLine("gt-drained", f.work.Rig, ReviewFindingText{
		ID: "f-3", Severity: "minor", File: "internal/land/note.go", Line: 30,
		Title: "swap the nil check", Claim: "the map is never nil"})); err != nil {
		t.Fatal(err)
	}
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, first, second)); err != nil {
		t.Fatal(err)
	}
	f.bd.Seed(
		beads.Issue{ID: "gt-drained", Title: "an earlier landing", Status: "closed", Type: "bug"},
		beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"},
	)
	// Two slots under the bound: one bead for the whole line, and one of the
	// two-package line's beads, which leaves its other finding unfiled.
	f.fake.existing = openFollowupBeads(DefaultFollowupBackpressure - 2)
	log := f.run()
	if len(f.fake.created) != 2 {
		t.Fatalf("filed %d beads from the queue, want one per line:\n%s", len(f.fake.created), log)
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Bead != "gt-held" {
		t.Fatalf("queue = %+v, want only the partly filed line left", queued)
	}
	// The next landing has room: the open count fell under the bound.
	for i := range f.fake.existing {
		f.fake.existing[i].Status = "closed"
	}
	log = f.run()
	if len(f.fake.created) != 3 {
		t.Fatalf("filed %d beads over both landings, want the second finding filed too:\n%s", len(f.fake.created), log)
	}
	if got := f.fake.created[2].Title; !strings.Contains(got, "count the row") {
		t.Errorf("the last bead is %q, want the finding the first landing left", got)
	}
	queued, err = f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("queue = %+v, want the line dropped once both findings were filed", queued)
	}
}

// TestFollowupFeedKeepsAnOverlongLineAndStillCompacts: a line past the
// compaction's bound may be a verdict, and the log only ever appends verdicts,
// so compaction keeps it as it was and says once that it skipped it — while the
// consumed queue line beside it is still dropped (gt-3arxj).
func TestFollowupFeedKeepsAnOverlongLineAndStillCompacts(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil)
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	// A small bound, so the case does not cost a 16 MiB line: the queue line
	// fits under it and the verdict below does not.
	f.reviews.lineLimit = 1024
	big, err := json.Marshal(ReviewRecord{BeadID: "gt-big", Rig: f.work.Rig, Verdict: VerdictApprove,
		Score: 0.9, At: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		FindingText: []ReviewFindingText{{File: "note.go", Line: 12, Title: "name the error", Claim: strings.Repeat("c", 2048)}}})
	if err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(f.reviews.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write(append(big, '\n')); err != nil {
		_ = fh.Close()
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
	log := f.run()
	if !strings.Contains(log, "dropped 1 consumed queue line") {
		t.Fatalf("log = %q, want the consumed queue line dropped", log)
	}
	if got := strings.Count(log, "too long to examine"); got != 1 {
		t.Errorf("log names the skipped line %d times, want one:\n%s", got, log)
	}
	raw, err := os.ReadFile(f.reviews.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), string(big)) {
		t.Errorf("the overlong verdict line is gone from the compacted log:\n%s", raw)
	}
	if queued, err := f.reviews.Queued(); err != nil || len(queued) != 0 {
		t.Errorf("queue = %+v, %v; want the drained line gone", queued, err)
	}
}

// TestCompactKeepsAQueueMarkedLineItCannotDecode: a line that marks itself a
// queue line but does not decode as one is kept, never dropped — compaction
// removes a line only when it can see the findings on it are covered, and what
// it cannot read it must not lose (gt-3arxj).
func TestCompactKeepsAQueueMarkedLineItCannotDecode(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	odd := `{"bead":"gt-odd","attempt":"not a number","at":"2026-10-20T12:00:00Z","followup_queue":{"findings":[]}}`
	done, err := json.Marshal(ReviewRecord{BeadID: "gt-done", Rig: "gastown", At: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		Queue: &FollowupQueue{Landed: queuedLanded, Score: 0.7, Findings: []ReviewFindingText{{Title: "done"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path, []byte(odd+"\n"+string(done)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := f.Compact(func(QueuedFollowup) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if res != (CompactResult{Dropped: 1}) {
		t.Fatalf("Compact = %+v, want only the decodable line dropped", res)
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != odd+"\n" {
		t.Errorf("compacted log =\n%s\nwant the undecodable line kept as it was", raw)
	}
}

// TestReviewRecordWithoutFindingTextStillParses: a count-only line, written
// before the record kept findings, still reads, and appending after it leaves
// it as it was (gt-3arxj).
func TestReviewRecordWithoutFindingTextStillParses(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"bead":"gt-p85pw.15","rig":"gastown","attempt":1,"score":0.8,"verdict":"approve","findings":4,"bar":0.7,"at":"2026-10-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(f.Path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := f.Since(time.Time{})
	if err != nil || len(recs) != 1 {
		t.Fatalf("Since on a count-only log = %+v, %v; want the one record", recs, err)
	}
	if recs[0].Findings != 4 || recs[0].FindingText != nil || recs[0].Verdict != VerdictApprove {
		t.Errorf("count-only record = %+v, want its count and no finding text", recs[0])
	}
	if err := f.Append(ReviewRecord{BeadID: "gt-new", Verdict: VerdictApprove, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), old) {
		t.Errorf("the count-only line was rewritten:\n%s", raw)
	}
}
