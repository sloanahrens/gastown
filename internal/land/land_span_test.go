package land

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
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

// reseed writes w's ready note onto the fixture's bead, so the block carries
// the submission stamp a refusal reads; mutate, when set, shapes the bead
// further for the kind of refusal the test drives.
func (f *landFixture) reseed(w Work, mutate func(*beads.Issue)) {
	f.t.Helper()
	is := beads.Issue{ID: "gt-abc", Title: "add b", Status: "hooked", Type: "task",
		Assignee: "gastown/polecats/opal", Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(w)}
	if mutate != nil {
		mutate(&is)
	}
	f.bd.Seed(is)
}

// reseat points the work at branch, committed on the fixture's main so the
// landing fetches and verifies it like any other, and names worker on the
// ready note.
func (f *landFixture) reseat(branch, worker string) {
	f.t.Helper()
	f.git.SetRef(f.t, f.origin, "refs/heads/"+branch, f.base)
	f.work.Branch = branch
	f.work.Head = f.git.Commit(f.t, f.origin, branch, "feat: add b", map[string]string{"b.txt": "work\n"})
	f.work.Worker = worker
	f.reseed(f.work, nil)
}

// spanLog is the bytes of the fixture's bead's polecat span log.
func (f *landFixture) spanLog() []byte {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(spans.DefaultDir(f.town), "gt-abc.jsonl"))
	if err != nil {
		f.t.Fatalf("reading the span log: %v", err)
	}
	return data
}

// refusedFixture opens a seat's attempt on the fixture's bead and stamps the
// submission the refusal answers.
func refusedFixture(t *testing.T) (*landFixture, *spans.Store) {
	t.Helper()
	f := newLandFixture(t)
	store := spans.New(spans.DefaultDir(f.town), &spanClock{at: spanEpoch})
	if err := store.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatalf("start span: %v", err)
	}
	f.work.Submitted = spanEpoch.Add(3 * time.Minute)
	f.reseed(f.work, nil)
	return f, store
}

// TestRefusedLandingEndsTheSeatsAttempt: a refusal returns the bead to its
// author, so the submission it answers was the seat's turn end, and the attempt
// its gt done left open is ended at the submission the host first saw. The
// Landings pane reads a rejected row the way it reads a landed one, so the row
// carries the seat's seconds instead of a dash (gt-fhjdh).
func TestRefusedLandingEndsTheSeatsAttempt(t *testing.T) {
	t.Parallel()
	f, store := refusedFixture(t)
	seedFirstSight(t, f.town, "gt-abc", f.work.Head, spanEpoch.Add(3*time.Minute+30*time.Second))
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2}}}
	}

	l := f.lander()
	l.Landings.Spans = store
	l.Now = func() time.Time { return spanEpoch.Add(5 * time.Minute) }
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectGate, LabelRework)

	// The reading the pane gives a rejected row: zero asOf, a landed record's
	// own reading (gt-kmp7c).
	secs, attempts, lower := l.Landings.PolecatTime("gt-abc", time.Time{})
	if secs != 180 || lower {
		t.Errorf("rejected row reads %v secs (lower bound %v), want the seat's 180 and no bound", secs, lower)
	}
	if len(attempts) != 1 || attempts[0].Seat != "gastown/opal" || attempts[0].Secs != 180 {
		t.Errorf("attempts = %+v, want the seat's 180", attempts)
	}
	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open || sum.Skipped != 0 {
		t.Errorf("summary = %+v, want one attempt closed by the refusal", sum)
	}
}

// TestRefusedLandingLeavesTheAttemptOpenWithoutAFirstSight: a refusal is not a
// turn end when the host holds no record of seeing the submission, and an end
// nothing vouches for is not written. The attempt stays open and the pane reads
// it as a lower bound, for every kind of refusal (gt-fhjdh).
func TestRefusedLandingLeavesTheAttemptOpenWithoutAFirstSight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		kind   RejectionKind
		mutate func(*beads.Issue)
		drive  func(*landFixture)
	}{
		{
			name: "conflict",
			kind: RejectConflict,
			drive: func(f *landFixture) {
				f.base = f.pushMain("b.txt", "main's b\n")
			},
		},
		{
			name: "gate",
			kind: RejectGate,
			drive: func(f *landFixture) {
				f.gate.fn = func(string) GateResult {
					return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2}}}
				}
			},
		},
		{
			name: "policy",
			kind: RejectPolicy,
			mutate: func(is *beads.Issue) {
				is.AcceptanceCriteria = "- [x] one\n- [ ] two\n"
			},
		},
		{
			name: "review",
			kind: RejectReview,
			drive: func(f *landFixture) {
				f.review.fn = func(string) (Verdict, error) {
					return Verdict{Verdict: VerdictRequestChanges, Score: 0.4}, nil
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, store := refusedFixture(t)
			if tc.mutate != nil {
				f.reseed(f.work, tc.mutate)
			}
			if tc.drive != nil {
				tc.drive(f)
			}
			l := f.lander()
			l.Landings.Spans = store
			l.Now = func() time.Time { return spanEpoch.Add(5 * time.Minute) }
			_, err := l.Land(context.Background(), f.work)
			f.assertRejected(t, err, tc.kind, LabelRework)

			secs, _, lower := l.Landings.PolecatTime("gt-abc", time.Time{})
			if secs != 0 || !lower {
				t.Errorf("rejected row reads %v secs (lower bound %v), want no seconds and a lower bound", secs, lower)
			}
			sum, err := store.Summarize("gt-abc")
			if err != nil {
				t.Fatalf("Summarize: %v", err)
			}
			if len(sum.Attempts) != 1 || !sum.Attempts[0].Open {
				t.Errorf("summary = %+v, want the seat's attempt left open", sum)
			}
		})
	}
}

// TestRefusedLandingLeavesTheAttemptOpenWhenTheFirstSightCannotBeRead: an
// unreadable first-sight log is not a sighting, so the refusal writes no end
// and the attempt stays open (gt-fhjdh).
func TestRefusedLandingLeavesTheAttemptOpenWhenTheFirstSightCannotBeRead(t *testing.T) {
	t.Parallel()
	f, store := refusedFixture(t)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2}}}
	}

	l := f.lander()
	l.Landings.Spans = store
	l.Landings.FirstSeen = func(string, string) (time.Time, error) {
		return time.Time{}, errors.New("unreadable first-sight log")
	}
	l.Now = func() time.Time { return spanEpoch.Add(5 * time.Minute) }
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectGate, LabelRework)

	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || !sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the seat's attempt left open", sum)
	}
}

// TestRefusedLandingLeavesTheAttemptOpenWhenTheBeadChangedHands: the bead was
// taken by someone else while the landing ran, so the refusal is noted and the
// bead left as it is. Whoever holds it now may keep working, so the attempt
// stays open rather than ending a turn that has not ended (gt-fhjdh).
func TestRefusedLandingLeavesTheAttemptOpenWhenTheBeadChangedHands(t *testing.T) {
	t.Parallel()
	f, store := refusedFixture(t)
	seedFirstSight(t, f.town, "gt-abc", f.work.Head, spanEpoch.Add(3*time.Minute+30*time.Second))
	const other = "gastown/polecats/nux"
	assignee := other
	f.gate.fn = func(string) GateResult {
		if err := f.bd.Update("gt-abc", beads.UpdateOptions{Assignee: &assignee}); err != nil {
			t.Errorf("handing the bead to %s: %v", other, err)
		}
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2}}}
	}

	l := f.lander()
	l.Landings.Spans = store
	l.Now = func() time.Time { return spanEpoch.Add(5 * time.Minute) }
	_, err := l.Land(context.Background(), f.work)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != RejectGate || rej.RecordErr == nil {
		t.Fatalf("Land = %T %v, want a gate rejection noting the bead changed hands", err, err)
	}

	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || !sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the seat's attempt left open", sum)
	}
}

// TestRefusedLandingEndsTheAttemptAtTheSubmissionNotTheRefusal: a refusal ten
// minutes after the seat submitted does not credit the seat those ten minutes.
// The attempt ends at the submission the host first saw, and the re-sling that
// answers the refusal opens a second attempt, so the reading carries the first
// attempt's seconds and a floor over the second (gt-fhjdh).
func TestRefusedLandingEndsTheAttemptAtTheSubmissionNotTheRefusal(t *testing.T) {
	t.Parallel()
	f, store := refusedFixture(t)
	submitted := spanEpoch.Add(10 * time.Second)
	f.work.Submitted = submitted
	f.reseed(f.work, nil)
	seedFirstSight(t, f.town, "gt-abc", f.work.Head, submitted)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2}}}
	}

	l := f.lander()
	l.Landings.Spans = store
	l.Now = func() time.Time { return spanEpoch.Add(10 * time.Minute) }
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectGate, LabelRework)

	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open || sum.Attempts[0].Secs != 10 {
		t.Fatalf("summary = %+v, want the attempt closed at the submission's 10 seconds", sum)
	}

	// The rework is slung again, and its attempt is open while the refused
	// one stays at the submission's seconds.
	clock := &spanClock{at: spanEpoch.Add(11 * time.Minute)}
	resling := spans.New(spans.DefaultDir(f.town), clock)
	if err := resling.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatalf("re-sling: %v", err)
	}
	secs, attempts, lower := l.Landings.PolecatTime("gt-abc", time.Time{})
	if secs != 10 || !lower {
		t.Errorf("reading = %v secs (lower bound %v), want the refused 10 and a bound over the rework", secs, lower)
	}
	if len(attempts) != 2 || attempts[0].Secs != 10 || attempts[1].Secs != 0 {
		t.Errorf("attempts = %+v, want the refused 10 and the open rework", attempts)
	}
}

// TestRefusedLandingLeavesACrewsOrBirdsAttemptAlone: a crew branch has no seat
// and a bird keeps the bead it claimed, so neither refusal ends an attempt. The
// span log is left byte for byte as it was (gt-fhjdh).
func TestRefusedLandingLeavesACrewsOrBirdsAttemptAlone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		branch string
		bird   bool
	}{
		{name: "crew branch", branch: "crew/merlin/gt-abc"},
		{name: "bird-owned bead", bird: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, store := refusedFixture(t)
			if tc.branch != "" {
				f.reseat(tc.branch, "merlin")
			}
			// The host saw this head, so only the seat rule can leave the
			// attempt alone: a close would write to the log below.
			seedFirstSight(t, f.town, "gt-abc", f.work.Head, spanEpoch.Add(4*time.Minute))
			before := f.spanLog()
			f.gate.fn = func(string) GateResult {
				return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2}}}
			}

			l := f.lander()
			l.Landings.Spans = store
			if tc.bird {
				l.BirdOwned = func(string) bool { return true }
			}
			l.Now = func() time.Time { return spanEpoch.Add(5 * time.Minute) }
			_, err := l.Land(context.Background(), f.work)
			var rej *Rejection
			if !errors.As(err, &rej) || rej.Kind != RejectGate || rej.RecordErr != nil {
				t.Fatalf("Land = %T %v, want a recorded gate rejection", err, err)
			}
			if after := f.spanLog(); string(after) != string(before) {
				t.Errorf("span log = %q, want it untouched", after)
			}
		})
	}
}

// TestRefusedLandingLeavesTheAttemptOpenWhenTheWorkerLineDisagrees: the ready
// note's worker line is the seat's own word, and a seat may not name the
// attempt its refusal ends. A line naming another polecat than the verified
// branch matches closes nothing (gt-fhjdh).
func TestRefusedLandingLeavesTheAttemptOpenWhenTheWorkerLineDisagrees(t *testing.T) {
	t.Parallel()
	f, store := refusedFixture(t)
	f.work.Worker = "nux"
	f.reseed(f.work, nil)
	seedFirstSight(t, f.town, "gt-abc", f.work.Head, spanEpoch.Add(4*time.Minute))
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2}}}
	}

	l := f.lander()
	l.Landings.Spans = store
	l.Now = func() time.Time { return spanEpoch.Add(5 * time.Minute) }
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectGate, LabelRework)

	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || !sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the seat's attempt left open", sum)
	}
}

// TestLandFloorsTheTimeASeatWithNoSpanStartLeft: gt-p85pw.92's shape — the seat
// wrote no span start at all, so the span log holds no clock for it. The
// landing times the record from the bead's sling in the event log to the
// submission it trusts, marked an upper bound rather than left a dash: the
// sling precedes the seat's session, so the interval may overstate its time but
// cannot understate it (gt-v7ptp).
func TestLandFloorsTheTimeASeatWithNoSpanStartLeft(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	seedSling(t, f.town, "gt-abc", spanEpoch)
	f.work.Submitted = spanEpoch.Add(20 * time.Minute)
	seedFirstSight(t, f.town, "gt-abc", f.work.Head, spanEpoch.Add(30*time.Minute))

	l := f.lander()
	// A span store the seat never wrote to.
	l.Landings.Spans = spans.New(spans.DefaultDir(f.town), nil)
	l.Reviewer = SkipReviewer{}
	l.Now = func() time.Time { return spanEpoch.Add(40 * time.Minute) }
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
	if rec.PolecatSecs != 20*60 {
		t.Errorf("polecat_secs = %v, want the 1200s from the sling to the submission", rec.PolecatSecs)
	}
	if !rec.PolecatUpperBound {
		t.Error("polecat_upper_bound unset on a time read from the dispatch")
	}
	if rec.PolecatLowerBound {
		t.Error("polecat_lower_bound set on a dispatch interval, which can overstate the seat's time")
	}
	want := []PolecatAttempt{{Seat: "gastown/opal", Secs: 20 * 60, UpperBound: true, From: PolecatFromSling}}
	if !reflect.DeepEqual(rec.PolecatAttempts, want) {
		t.Errorf("polecat_attempts = %+v, want %+v", rec.PolecatAttempts, want)
	}
}
