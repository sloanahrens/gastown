package land

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testutil"
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
		ID: "f-3", Severity: "minor", File: "internal/cmd/note.go", Line: 30,
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
	if got := f.fake.created[2].Body; !strings.Contains(got, "count the row") {
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

// privateLandingsPath is the path of a landings file in a directory only this
// account can write: ensureLandingsDir refuses a landings dir other accounts
// can write (gt-aq8dej).
func privateLandingsPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(testutil.PrivateTempDir(t), "landings.jsonl")
}

// budgetWeek is a landings file with n records landed inside the seven-day
// window the follow-up budget counts.
func budgetWeek(t *testing.T, n int) *LandingsFile {
	t.Helper()
	f := &LandingsFile{Path: privateLandingsPath(t)}
	at := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < n; i++ {
		if err := f.Append(LandingRecord{BeadID: fmt.Sprintf("gt-week-%d", i), LandedAt: at}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	return f
}

// filedThisWeek is n follow-up beads filed inside the window, the count the
// budget measures its share against.
func filedThisWeek(n int) []ExistingFollowup {
	at := time.Now().UTC().Add(-time.Hour)
	out := make([]ExistingFollowup, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ExistingFollowup{ID: fmt.Sprintf("gt-filed-week-%d", i), Title: "an older finding", Status: "open", CreatedAt: at})
	}
	return out
}

// TestFollowupBudgetFeedHoldsAPlanAndQueuesIt: a plan that would carry the
// week's follow-up beads past their share of its landings is not filed. Its
// findings go to the same queue the backpressure bound uses, and the line says
// "weekly budget" so the hold is not read as backpressure (gt-y5wvum).
func TestFollowupBudgetFeedHoldsAPlanAndQueuesIt(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	// Five beads filed for eight landings: a sixth bead is over 25 percent.
	f.lander.Landings = budgetWeek(t, 8)
	f.fake.existing = filedThisWeek(5)
	log := f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads over the weekly budget:\n%s", len(f.fake.created), log)
	}
	if !strings.Contains(log, "weekly budget") {
		t.Fatalf("log = %q, want the weekly budget named", log)
	}
	if strings.Contains(log, "backpressure bound") {
		t.Errorf("log = %q, want the budget hold not read as backpressure", log)
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || len(queued[0].Findings) != 1 || queued[0].Findings[0].Title != "name the error" {
		t.Errorf("queue = %+v, want the held finding", queued)
	}
}

// TestFollowupBudgetFeedDrainDoesNotNameTheBound: a queue line the weekly budget
// holds back again is not read as backpressure. fileFollowupBeads names the
// budget, and the drain's line about the queue names no cause (gt-9r0owg).
func TestFollowupBudgetFeedDrainDoesNotNameTheBound(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, nil) // nothing of its own to file
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
	// Five beads filed for eight landings: the drain's bead would be the sixth,
	// over 25 percent.
	f.lander.Landings = budgetWeek(t, 8)
	f.fake.existing = filedThisWeek(5)
	log := f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads over the weekly budget:\n%s", len(f.fake.created), log)
	}
	if !strings.Contains(log, "weekly budget") {
		t.Fatalf("log = %q, want the budget hold named", log)
	}
	if strings.Contains(log, "the bound held") {
		t.Errorf("log = %q, want the budget hold not named as the bound", log)
	}
	if !strings.Contains(log, "the feed held 1 finding of the queue on gt-held back again") {
		t.Errorf("log = %q, want the queue line's hold named", log)
	}
}

// TestFollowupBudgetFeedDrainsWhenThereIsRoom: the queue waits for the week to
// have room again, and the landing that finds it files the held findings
// (gt-y5wvum).
func TestFollowupBudgetFeedDrainsWhenThereIsRoom(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	f.lander.Landings = budgetWeek(t, 8)
	f.fake.existing = filedThisWeek(5)
	if log := f.run(); len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads over the weekly budget:\n%s", len(f.fake.created), log)
	}
	// The later landing has nothing of its own to file; the week now has 24
	// landings, so a sixth bead is at 25 percent rather than over it.
	f.result.Verdict.Findings = nil
	at := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < 16; i++ {
		if err := f.lander.Landings.Append(LandingRecord{BeadID: fmt.Sprintf("gt-later-%d", i), LandedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	log := f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads with room in the week's budget, want the held one:\n%s", len(f.fake.created), log)
	}
	if body := f.fake.created[0].Body; !strings.Contains(body, "name the error") {
		t.Errorf("bead body lacks the held finding:\n%s", body)
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("queue = %+v, want the drained line gone", queued)
	}
}

// TestFollowupBudgetFeedCountsTheBeadsItFiledThisPass: a bead the queue drain
// files counts in the week's filed total for the landing's own findings after
// it, so one landing cannot reach the floor with a fresh count and file past
// its share (gt-y5wvum).
func TestFollowupBudgetFeedCountsTheBeadsItFiledThisPass(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/metrics/panel.go", 40, "count the row")})
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
	// Four beads under the floor of five: the drain files the fifth, and the
	// landing's own finding is the sixth, over 25 percent of eight landings.
	f.lander.Landings = budgetWeek(t, 8)
	f.fake.existing = filedThisWeek(4)
	log := f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want the held one from the queue and none over budget:\n%s", len(f.fake.created), log)
	}
	if !strings.Contains(log, "weekly budget") {
		t.Errorf("log = %q, want the own finding held by the weekly budget", log)
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || len(queued[0].Findings) != 1 || queued[0].Findings[0].Title != "count the row" {
		t.Errorf("queue = %+v, want the own finding queued", queued)
	}
}

// TestFollowupBudgetFeedNeedsLandings: a Lander with no landings file wired
// files with no weekly budget; a landings file that exists and cannot be read
// says nothing about room, so the configured budget stands with the week's
// landings at none and only the floor is filed — the rest is held and queued
// (gt-y5wvum, gt-5hyv80).
func TestFollowupBudgetFeedNeedsLandings(t *testing.T) {
	t.Parallel()
	t.Run("no landings file", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.fake.existing = filedThisWeek(5) // over any budget, if one were applied
		log := f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads without a landings file, want the plan filed anyway:\n%s", len(f.fake.created), log)
		}
		if !strings.Contains(log, "filing without a weekly budget") {
			t.Errorf("log = %q, want the missing budget named", log)
		}
	})
	t.Run("a landings file that cannot be read", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.lander.Landings = &LandingsFile{Path: t.TempDir()}
		f.fake.existing = filedThisWeek(5) // five already this week is the floor
		log := f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads on an unreadable landings file past the floor, want the plan held:\n%s", len(f.fake.created), log)
		}
		if !strings.Contains(log, "could not read the week's landings") {
			t.Errorf("log = %q, want the unreadable landings named", log)
		}
		if !strings.Contains(log, "holding past the first 5 beads") {
			t.Errorf("log = %q, want the hold named", log)
		}
		queued, err := f.reviews.Queued()
		if err != nil {
			t.Fatal(err)
		}
		if len(queued) != 1 || len(queued[0].Findings) != 1 || queued[0].Findings[0].Title != "name the error" {
			t.Errorf("queue = %+v, want the held finding", queued)
		}
	})
	t.Run("a landings file that cannot be read under the floor", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.lander.Landings = &LandingsFile{Path: t.TempDir()}
		f.fake.existing = filedThisWeek(3) // under the floor, so a broken log does not starve a quiet week
		log := f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads on an unreadable landings file under the floor, want the plan filed:\n%s", len(f.fake.created), log)
		}
		if !strings.Contains(log, "could not read the week's landings") {
			t.Errorf("log = %q, want the unreadable landings named", log)
		}
	})
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

// TestQueueKeepsTheClass: a held finding's class goes through the queue line and
// reads back with it, and a line written before the chain carried a class reads
// back empty, which files as behaviour (gt-iwyb13).
func TestQueueKeepsTheClass(t *testing.T) {
	t.Parallel()
	held := followupCandidates([]Finding{{ID: "f1", Severity: "minor", Path: "a.go", Title: "t", Detail: "d", Class: FindingClassTestStyle}})
	line, err := json.Marshal(heldFindingTexts(held))
	if err != nil {
		t.Fatal(err)
	}
	old := `[{"id":"f0","severity":"minor","file":"a.go","title":"t","claim":"c"}]`
	for _, tc := range []struct{ name, json, want string }{
		{"a held test_style finding", string(line), FindingClassTestStyle},
		{"an old queue line", old, ""},
	} {
		var texts []ReviewFindingText
		if err := json.Unmarshal([]byte(tc.json), &texts); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := queuedFindings(texts); len(got) != 1 || got[0].Class != tc.want {
			t.Errorf("%s: read back %+v, want one finding of class %q", tc.name, got, tc.want)
		}
	}
}

// queuedDetail is queuedFinding as a filed body's Goal bullet renders it.
const queuedDetail = "name the error (at internal/land/note.go:12, om finding f-9, minor)"

// touchedNote is what the Goal bullet of a finding replayed after its file
// changed since the landing ends with: the commits that touched the file
// (gt-vn8dhf).
func touchedNote(shas ...string) string {
	return "; the file changed since the landing in " + strings.Join(shas, ", ")
}

// touchedBox is the acceptance box of a finding replayed after its file changed:
// it accepts the finding as already fixed on origin/main (gt-vn8dhf).
func touchedBox(shas ...string) string {
	return "- [ ] Finding resolved, or already fixed on origin/main (the file changed in " +
		strings.Join(shas, ", ") + "): name the error (internal/land/note.go:12)"
}

// plainBox is the acceptance box of a finding whose file is not known to have
// changed since the landing.
const plainBox = "- [ ] Finding resolved: name the error (internal/land/note.go:12)"

// goalBullet is the one Goal bullet of a filed body, so a test asserts the
// wording of the line a worker reads rather than the body as a whole.
func goalBullet(t *testing.T, body string) string {
	t.Helper()
	goal, _, _ := strings.Cut(body, "\n## Constraints")
	var got []string
	for _, line := range strings.Split(goal, "\n") {
		if strings.HasPrefix(line, "- ") {
			got = append(got, line)
		}
	}
	if len(got) != 1 {
		t.Fatalf("the Goal carries %d bullets, want one:\n%s", len(got), body)
	}
	return got[0]
}

// queuedReplayFixture is a landing with nothing of its own to file and room
// under the bound, replaying one queue line of queuedFinding with the lander's
// PathTouched set to hook (nil for none).
func queuedReplayFixture(t *testing.T, hook func(landed, base, path string) []string) *followupFixture {
	t.Helper()
	f := newFollowupFixture(t, 0.9, nil)
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
	f.lander.PathTouched = hook
	f.fake.existing = openFollowupBeads(1)
	return f
}

// TestFollowupTouched: a queued finding replayed after its file changed since
// the landing says so in the filed body. With PathTouched answering two commits
// the Goal bullet ends with them and the acceptance box accepts the finding as
// already fixed on origin/main; with no hook, an empty answer, a panic or a
// blank sha the body is unchanged; and a landing's own finding is never marked,
// because only a replay can be out of date with the file (gt-vn8dhf).
func TestFollowupTouched(t *testing.T) {
	t.Parallel()
	const (
		shaA = "aaa1111"
		shaB = "bbb2222"
		path = "internal/land/note.go"
	)
	// unchanged is the body a replay files when its file is not known to have
	// changed: the plain bullet naming the landing it came from, and the plain
	// box.
	unchanged := func(t *testing.T, f *followupFixture) {
		t.Helper()
		log := f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads, want one:\n%s", len(f.fake.created), log)
		}
		created := f.fake.created[0]
		if want := "- " + queuedDetail + " (from gt-held, landed " + queuedLanded + ")"; goalBullet(t, created.Body) != want {
			t.Errorf("Goal bullet = %q, want %q", goalBullet(t, created.Body), want)
		}
		if created.Acceptance != plainBox {
			t.Errorf("Acceptance = %q, want %q", created.Acceptance, plainBox)
		}
	}
	t.Run("two commits", func(t *testing.T) {
		var calls [][3]string
		f := queuedReplayFixture(t, func(landed, base, p string) []string {
			calls = append(calls, [3]string{landed, base, p})
			return []string{shaA, shaB}
		})
		log := f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads, want one:\n%s", len(f.fake.created), log)
		}
		if len(calls) == 0 {
			t.Fatal("PathTouched was never asked")
		}
		// The hook is handed the queue line's landing and this landing's base,
		// the range the commits that touched the file are read from.
		for _, c := range calls {
			if c != [3]string{queuedLanded, "base", path} {
				t.Errorf("PathTouched(%q, %q, %q), want (%q, %q, %q)", c[0], c[1], c[2], queuedLanded, "base", path)
			}
		}
		created := f.fake.created[0]
		bullet := goalBullet(t, created.Body)
		if !strings.HasSuffix(bullet, touchedNote(shaA, shaB)) {
			t.Errorf("Goal bullet = %q, want it to end with %q", bullet, touchedNote(shaA, shaB))
		}
		if !strings.Contains(bullet, "(from gt-held, landed "+queuedLanded+")") {
			t.Errorf("Goal bullet = %q, want the landing it came from kept", bullet)
		}
		if want := touchedBox(shaA, shaB); created.Acceptance != want {
			t.Errorf("Acceptance = %q, want %q", created.Acceptance, want)
		}
	})
	t.Run("no PathTouched", func(t *testing.T) {
		unchanged(t, queuedReplayFixture(t, nil))
	})
	t.Run("PathTouched answers none", func(t *testing.T) {
		f := queuedReplayFixture(t, func(string, string, string) []string { return nil })
		unchanged(t, f)
	})
	t.Run("PathTouched answers only blanks", func(t *testing.T) {
		f := queuedReplayFixture(t, func(string, string, string) []string { return []string{"", "  "} })
		unchanged(t, f)
	})
	t.Run("a blank sha beside a real one is dropped", func(t *testing.T) {
		f := queuedReplayFixture(t, func(string, string, string) []string { return []string{"", shaA} })
		f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads, want one", len(f.fake.created))
		}
		if bullet, want := goalBullet(t, f.fake.created[0].Body), touchedNote(shaA); !strings.HasSuffix(bullet, want) {
			t.Errorf("Goal bullet = %q, want it to end with %q", bullet, want)
		}
	})
	t.Run("PathTouched panics", func(t *testing.T) {
		f := queuedReplayFixture(t, func(string, string, string) []string { panic("the hook broke") })
		unchanged(t, f)
	})
	t.Run("the skip hook sees no mark", func(t *testing.T) {
		f := queuedReplayFixture(t, func(string, string, string) []string { return []string{shaA, shaB} })
		var seen []int
		f.lander.FollowupSkip = func(_ Work, c FollowupCandidate) string {
			seen = append(seen, len(c.Touched))
			return ""
		}
		f.run()
		if len(seen) == 0 {
			t.Fatal("the skip hook was never asked")
		}
		for _, n := range seen {
			if n != 0 {
				t.Errorf("the skip hook saw %d touched commits, want none: the mark is set after it answers", n)
			}
		}
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads, want one", len(f.fake.created))
		}
		if bullet, want := goalBullet(t, f.fake.created[0].Body), touchedNote(shaA, shaB); !strings.HasSuffix(bullet, want) {
			t.Errorf("Goal bullet = %q, want it to end with %q", bullet, want)
		}
	})
	t.Run("a landing's own finding is not marked", func(t *testing.T) {
		var calls int
		f := newFollowupFixture(t, 0.9, []Finding{{ID: "f-9", Severity: "minor", Path: path, Line: 12,
			Title: "name the error", Detail: queuedClaim}})
		f.lander.PathTouched = func(string, string, string) []string {
			calls++
			return []string{shaA, shaB}
		}
		f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads, want one", len(f.fake.created))
		}
		if calls != 0 {
			t.Errorf("PathTouched was asked %d times about a landing's own finding, want never", calls)
		}
		if body := f.fake.created[0].Body; strings.Contains(body, "file changed") {
			t.Errorf("the body of a landing's own finding marks a file change:\n%s", body)
		}
	})
	t.Run("appended to an open bundle", func(t *testing.T) {
		f := queuedReplayFixture(t, func(string, string, string) []string { return []string{shaA, shaB} })
		f.fake.existing = append(f.fake.existing, openBundleFor(followupDay, path, "open"))
		f.run()
		if len(f.fake.created) != 0 || len(f.fake.appended) != 1 {
			t.Fatalf("created %d, appended %d, want the finding appended to the open bundle", len(f.fake.created), len(f.fake.appended))
		}
		add := f.fake.appended[0].Add
		if len(add.Lines) != 1 || !strings.HasSuffix(add.Lines[0], touchedNote(shaA, shaB)) {
			t.Errorf("appended lines = %q, want one ending with %q", add.Lines, touchedNote(shaA, shaB))
		}
		if len(add.Boxes) != 1 || add.Boxes[0] != touchedBox(shaA, shaB) {
			t.Errorf("appended boxes = %q, want %q", add.Boxes, touchedBox(shaA, shaB))
		}
	})
}

// queuedFeed runs the feed over a landing with nothing of its own to file and
// one queue line of findings that an earlier landing held back, against the
// follow-ups already open in existing (gt-bsyzhk).
func queuedFeed(t *testing.T, existing []ExistingFollowup, findings ...ReviewFindingText) (*followupFixture, string) {
	t.Helper()
	f := newFollowupFixture(t, 0.9, nil)
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, findings...)); err != nil {
		t.Fatal(err)
	}
	f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
	f.fake.existing = existing
	return f, f.run()
}

// queuedTexts is n queue-line findings of one package, titled "queued 1" to "queued n",
// of class behaviour set explicitly: a row that needs the class-less default says so
// itself (TestQueueReplayOldLineIsBehaviour).
func queuedTexts(n int) []ReviewFindingText {
	var out []ReviewFindingText
	for i := 1; i <= n; i++ {
		out = append(out, ReviewFindingText{ID: fmt.Sprintf("q-%d", i), Severity: "minor", File: "internal/land/note.go", Line: i, Title: fmt.Sprintf("queued %d", i), Claim: queuedClaim, Class: FindingClassBehaviour})
	}
	return out
}

// TestQueueReplayHoldsAtTheBound: with the open count at the bound and no open
// bundle to append to, a held finding stays in the review log and nothing is
// filed or appended; the next landing that has room replays it (gt-bsyzhk).
func TestQueueReplayHoldsAtTheBound(t *testing.T) {
	t.Parallel()
	f, log := queuedFeed(t, openFollowupBeads(DefaultFollowupBackpressure), queuedTexts(1)...)
	if len(f.fake.created) != 0 || len(f.fake.appended) != 0 {
		t.Errorf("created %d, appended %d at the bound, want neither:\n%s", len(f.fake.created), len(f.fake.appended), log)
	}
	if queued, err := f.reviews.Queued(); err != nil || len(queued) != 1 {
		t.Errorf("queue = %+v (%v), want the line kept for a landing with room", queued, err)
	}
}

// TestQueueReplayFilesIntoTheBundle: a queued finding replayed when the bound has
// room goes into the day's bundle, and the consumed line is dropped: filed as a
// new bundle when none is open, appended to the open one when there is, and, past
// the room of one bundle, as bundles of at most feedChunkCap findings (gt-ntkwh0).
func TestQueueReplayFilesIntoTheBundle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		existing     []ExistingFollowup
		n            int
		wantAppended int
		wantCreated  []int
	}{
		{"a new bundle", openFollowupBeads(1), 1, 0, []int{1}},
		{"a full bundle is not appended to", append(openFollowupBeads(1), bundleWithFindings(3)), 2, 0, []int{2}},
		{"findings past one chunk, no open bundle", openFollowupBeads(1), feedChunkCap + 2, 0, chunkSizes(feedChunkCap + 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, log := queuedFeed(t, tc.existing, queuedTexts(tc.n)...)
			appended := 0
			for _, a := range f.fake.appended {
				appended += len(a.Add.Labels)
			}
			var created []int
			for _, spec := range f.fake.created {
				created = append(created, fingerprints(spec.Labels))
			}
			if appended != tc.wantAppended || !slices.Equal(created, tc.wantCreated) {
				t.Errorf("appended %d, created bundles of %v, want %d and %v:\n%s", appended, created, tc.wantAppended, tc.wantCreated, log)
			}
			if queued, err := f.reviews.Queued(); err != nil || len(queued) != 0 {
				t.Errorf("queue = %+v (%v), want the consumed line dropped", queued, err)
			}
		})
	}
}

// TestQueueReplayOldLineIsBehaviour: a queue line written before om gave a class
// carries findings with none, and replays as behaviour: it files a bead, where a
// line whose findings are class comment is noted and files none (gt-bsyzhk).
func TestQueueReplayOldLineIsBehaviour(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		class       string
		wantCreated int
	}{
		{"an old line with no class", "", 1},
		{"a line of class comment", "comment", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text := queuedTexts(1)
			text[0].Class = tc.class
			f, log := queuedFeed(t, openFollowupBeads(1), text...)
			if len(f.fake.created) != tc.wantCreated {
				t.Errorf("created %d beads, want %d:\n%s", len(f.fake.created), tc.wantCreated, log)
			}
			// A queued comment-class line is a leftover: replay consumes it and
			// writes no note, the note was written at the landing that raised the
			// finding (followup.go: splitNoted at line 204 splits it off before it
			// can be queued, AddComment at line 271 writes the note on that
			// landing's own bead, and queuedCandidates at line 596 discards the
			// noted ones). Asserting a note at replay would pin a double write.
			if tc.class == "comment" {
				if queued, err := f.reviews.Queued(); err != nil || len(queued) != 0 {
					t.Errorf("queue = %+v (%v), want the leftover comment line consumed", queued, err)
				}
				if comments, err := f.bd.Comments("gt-held"); err != nil || len(comments) != 0 {
					t.Errorf("comments on gt-held = %v (%v), want none written at replay", commentTexts(comments), err)
				}
			}
		})
	}
}
