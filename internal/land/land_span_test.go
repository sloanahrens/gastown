package land

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/spans"
)

// TestLandWritesTheEndASandboxedSeatCouldNot: a sandboxed seat starts its
// attempt through the sling on the host but runs gt done in the sandbox's own
// town, so the end it writes there is deleted with the sandbox. The landing
// writes that end from the submission the seat recorded, so the record carries
// the seat's seconds instead of a dash (gt-8yk78).
func TestLandWritesTheEndASandboxedSeatCouldNot(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	store := spans.New(spans.DefaultDir(f.town), &spanClock{at: spanEpoch})
	if err := store.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatalf("start span: %v", err)
	}
	f.work.Submitted = spanEpoch.Add(3 * time.Minute)
	seedFirstSight(t, f.town, "gt-abc", f.work.Head, spanEpoch.Add(3*time.Minute+30*time.Second))

	l := f.lander()
	l.Landings.Spans = store
	l.Reviewer = SkipReviewer{}
	// The landing happens after the seat submitted: that time bounds the end
	// the landing writes on the seat's behalf (gt-8yk78).
	l.Now = func() time.Time { return spanEpoch.Add(4 * time.Minute) }
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}

	var rec LandingRecord
	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landing records = %q, want one", lines)
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decoding the landing record: %v", err)
	}
	if rec.PolecatSecs != 180 {
		t.Errorf("polecat_secs = %v, want the seat's 180 seconds", rec.PolecatSecs)
	}
	if rec.PolecatLowerBound {
		t.Error("polecat_lower_bound set on an attempt the landing closed")
	}
	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the seat's attempt closed", sum)
	}
}

// TestLandClosesNothingForASeatThatEndedItsOwnAttempt: a host seat's gt done
// already ended the attempt, so the landing writes no second end and the
// record reads the same seconds (gt-liseq).
func TestLandClosesNothingForASeatThatEndedItsOwnAttempt(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	store := spans.New(spans.DefaultDir(f.town), &spanClock{at: spanEpoch})
	if err := store.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatalf("start span: %v", err)
	}
	if err := store.End("gt-abc", "gastown/opal", spans.ReasonDone, spanEpoch.Add(2*time.Minute)); err != nil {
		t.Fatalf("end span: %v", err)
	}
	f.work.Submitted = spanEpoch.Add(3 * time.Minute)
	seedFirstSight(t, f.town, "gt-abc", f.work.Head, spanEpoch.Add(4*time.Minute))

	l := f.lander()
	l.Landings.Spans = store
	l.Reviewer = SkipReviewer{}
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}

	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landing records = %q, want one", lines)
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decoding the landing record: %v", err)
	}
	if rec.PolecatSecs != 120 {
		t.Errorf("polecat_secs = %v, want the 120 the seat ended at", rec.PolecatSecs)
	}
	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Skipped != 0 || len(sum.Attempts) != 1 || sum.Attempts[0].Secs != 120 {
		t.Errorf("summary = %+v, want one attempt of 120 and no second end", sum)
	}
}

// TestLandClosesTheSeatTheBranchNamesNotTheNotesWorkerLine: the ready note's
// worker line is the seat's own word, and a seat must not be able to end
// another seat's attempt. The seat is read from the branch the landing fetched
// and verified, so the note naming nux closes nothing of nux's (gt-8yk78).
func TestLandClosesTheSeatTheBranchNamesNotTheNotesWorkerLine(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	store := spans.New(spans.DefaultDir(f.town), &spanClock{at: spanEpoch})
	for _, seat := range []string{"gastown/opal", "gastown/nux"} {
		if err := store.Start("gt-abc", seat); err != nil {
			t.Fatalf("start span for %s: %v", seat, err)
		}
	}
	f.work.Worker = "nux"
	f.work.Submitted = spanEpoch.Add(2 * time.Minute)
	seedFirstSight(t, f.town, "gt-abc", f.work.Head, spanEpoch.Add(2*time.Minute))

	l := f.lander()
	l.Landings.Spans = store
	l.Reviewer = SkipReviewer{}
	l.Now = func() time.Time { return spanEpoch.Add(3 * time.Minute) }
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}

	var rec LandingRecord
	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landing records = %q, want one", lines)
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decoding the landing record: %v", err)
	}
	if rec.PolecatSecs != 120 {
		t.Errorf("polecat_secs = %v, want the branch seat's 120", rec.PolecatSecs)
	}
	if !rec.PolecatLowerBound {
		t.Error("polecat_lower_bound unset while nux's attempt is still open")
	}
	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	want := []spans.Attempt{{Seat: "gastown/opal", Secs: 120}, {Seat: "gastown/nux", Open: true}}
	if len(sum.Attempts) != len(want) {
		t.Fatalf("summary = %+v, want %+v", sum, want)
	}
	for i, a := range sum.Attempts {
		if a.Seat != want[i].Seat || a.Secs != want[i].Secs || a.Open != want[i].Open {
			t.Errorf("attempt %d = %+v, want %+v", i, a, want[i])
		}
	}
}

// TestLandLeavesTheAttemptOpenWithoutAFirstSightRecord: a submission stamp is
// not evidence the seat worked the time it claims, and the host has no record
// that it ever saw the submission. The landing writes no end, so the record
// keeps the lower bound rather than the seat's word for its hours (gt-8yk78).
func TestLandLeavesTheAttemptOpenWithoutAFirstSightRecord(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	store := spans.New(spans.DefaultDir(f.town), &spanClock{at: spanEpoch})
	if err := store.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatalf("start span: %v", err)
	}
	f.work.Submitted = spanEpoch.Add(2 * time.Minute)

	l := f.lander()
	l.Landings.Spans = store
	l.Reviewer = SkipReviewer{}
	l.Now = func() time.Time { return spanEpoch.Add(3 * time.Minute) }
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}

	var rec LandingRecord
	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landing records = %q, want one", lines)
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decoding the landing record: %v", err)
	}
	if rec.PolecatSecs != 0 || !rec.PolecatLowerBound {
		t.Errorf("polecat time = %v (lower bound %v), want none and a lower bound", rec.PolecatSecs, rec.PolecatLowerBound)
	}
	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || !sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the attempt left open", sum)
	}
}
