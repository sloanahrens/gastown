package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// readyBlock is the READY TO LAND block gt done writes, rendered by the code
// that writes it: the note that leads the gt:ready-to-land label by one bd
// write (internal/done markReadyToLand).
func readyBlock(branch, head, target string) string {
	return land.FormatReadyNote(land.Work{Branch: branch, Head: head, Target: target})
}

// midSubmissionIssue is a work bead in the submit->land window as a read can
// catch it: the READY TO LAND block written, the gt:ready-to-land label not
// yet. The label is what Eligible's excluded-label check reads, so the block is
// the only signal the answer carries (gt-kr5xv).
func midSubmissionIssue(id string) *beads.Issue {
	return &beads.Issue{
		ID: id, Type: "task", Status: "open", Priority: 1,
		Description:        specTestDescription,
		AcceptanceCriteria: "- [ ] a\n- [ ] b\n- [ ] c",
		Notes:              readyBlock("sloan/x", "abc1234", "main"),
	}
}

// A bead whose notes were caught mid-submission is not a candidate: the board
// answer omits gt:ready-to-land, and the block is what holds it (gt-kr5xv).
func TestSpecCandidatesDropsAMidSubmissionBead(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	issue := midSubmissionIssue("gt-mid")
	if specFromIssue(issue).HasLabel(land.LabelReadyToLand) {
		t.Fatal("the fixture wears the label; the window being pinned is the label-less half")
	}
	board := func(string) ([]*beads.Issue, error) { return []*beads.Issue{issue}, nil }

	got := specCandidates(townRoot, 2, board, nil)
	if len(got.Errors) != 0 {
		t.Fatalf("errors = %v", got.Errors)
	}
	if len(got.Candidates) != 0 {
		t.Fatalf("candidates = %v, want none: the bead is mid-submission", got.Candidates)
	}
}

// The board read lags the note write and the full re-read catches it: nothing
// is slung even though the board admitted the bead (gt-kr5xv).
func TestSpecDispatchHoldsABeadTheReReadCatchesMidSubmission(t *testing.T) {
	t.Parallel()
	stale := specFromIssue(&beads.Issue{
		ID: "gt-mid", Type: "task", Status: "open", Priority: 1,
		Description:        specTestDescription,
		AcceptanceCriteria: "- [ ] a\n- [ ] b\n- [ ] c",
	})
	f := newFakeSpecTown(stale)
	f.showOverride = map[string]specdispatch.Spec{"gt-mid": specFromIssue(midSubmissionIssue("gt-mid"))}

	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || len(r.Dispatched) != 0 {
		t.Fatalf("slung a bead mid-submission: slung %v report %+v", f.slung, r)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "submitted for landing") {
		t.Fatalf("skipped = %+v, want the mid-submission hold named", r.Skipped)
	}
}

// A landing request holds only while nothing has settled it. The LANDING RECORD
// a land writes and the MERGE REJECTION a rejection writes both close it, so a
// landed bead and a rework are the dispatcher's again (gt-kr5xv).
func TestSpecLiveLandingRequestStates(t *testing.T) {
	t.Parallel()
	ready := readyBlock("sloan/x", "abc1234", "main")
	rejection := land.MergeRejectionNoteMarker + " (attempt 1): tests failed"
	landing := land.LandingNoteMarker + "\nlanded_commit: def5678"
	for _, tc := range []struct {
		name  string
		notes string
		want  bool
	}{
		{name: "a block with nothing after it is live", notes: ready, want: true},
		{name: "a later landing record settles it", notes: ready + "\n\n" + landing},
		{name: "a later rejection settles it", notes: ready + "\n\n" + rejection},
		{name: "a resubmission after the rejection is live", notes: ready + "\n\n" + rejection + "\n\n" + ready, want: true},
		{name: "an incomplete block is no request", notes: land.ReadyNoteMarker + "\nBranch: sloan/x"},
		{name: "notes that only name the block are no request", notes: "a note about the READY TO LAND marker"},
		{name: "no notes are no request", notes: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := specLiveLandingRequest(tc.notes); got != tc.want {
				t.Fatalf("specLiveLandingRequest(%q) = %v, want %v", tc.notes, got, tc.want)
			}
		})
	}
}

// The window the tick's own read cannot close: a bead that changes after the
// tick read it and before the seat is spent. The last-moment re-read drops the
// candidate, so no polecat is slung onto work that is already landing or
// already someone else's (gt-01gix, gt-ue13q).
func TestSpecDispatchHoldsABeadThatChangesBeforeTheSling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(specdispatch.Spec) specdispatch.Spec
		want   string
	}{
		{
			name: "a submission lands",
			change: func(s specdispatch.Spec) specdispatch.Spec {
				return specFromIssue(midSubmissionIssue(s.ID))
			},
			want: "submitted for landing",
		},
		{
			name: "the ready label lands",
			change: func(s specdispatch.Spec) specdispatch.Spec {
				s.Labels = append(s.Labels, land.LabelReadyToLand)
				return s
			},
			want: "gt:ready-to-land",
		},
		{
			name: "someone else takes it",
			change: func(s specdispatch.Spec) specdispatch.Spec {
				s.Assignee = "gastown/polecats/malachite"
				return s
			},
			want: "assigned to gastown/polecats/malachite",
		},
		{
			// The steward parks the bead for a person a second after the tick
			// read it. A hold is a change like any other: the re-read has to
			// drop the candidate, or a polecat is spawned onto work the town
			// reserved for a human (gt-0k7kb).
			name: "the steward holds it",
			change: func(s specdispatch.Spec) specdispatch.Spec {
				s.Labels = append(s.Labels, "needs-human")
				return s
			},
			want: "needs-human",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clean := cleanSpec("gt-race", 1, "2026-09-30T11:00:00Z")
			f := newFakeSpecTown(clean)
			f.showSeq["gt-race"] = []specRead{{spec: clean}, {spec: tc.change(clean)}}

			r := runSpecDispatchCycle(f.env())
			if len(f.slung) != 0 || len(r.Dispatched) != 0 {
				t.Fatalf("slung a bead that changed mid-tick: slung %v report %+v", f.slung, r)
			}
			if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, specStaleReason) || !strings.Contains(r.Skipped[0].Line, tc.want) {
				t.Fatalf("skipped = %+v, want %q under the stale reason", r.Skipped, tc.want)
			}
		})
	}
}

// The re-read admits the same bead it read before: the extra read is a
// precondition, not a second gate on a bead that did not move (gt-01gix).
func TestSpecDispatchSlingsABeadTheReReadStillAdmits(t *testing.T) {
	t.Parallel()
	clean := cleanSpec("gt-still", 1, "2026-09-30T11:00:00Z")
	f := newFakeSpecTown(clean)
	f.showSeq["gt-still"] = []specRead{{spec: clean}, {spec: clean}}

	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.slung[0] != "gt-still" || len(r.Dispatched) != 1 || len(r.Skipped) != 0 {
		t.Fatalf("slung %v skipped %+v report %+v, want the unchanged bead taken", f.slung, r.Skipped, r)
	}
}

// A re-read that fails leaves the bead's state unknown, and unknown is not
// eligible: the dispatch waits for the next tick instead of slinging blind
// (gt-01gix).
func TestSpecDispatchHoldsWhenTheReReadFails(t *testing.T) {
	t.Parallel()
	clean := cleanSpec("gt-blind", 1, "2026-09-30T11:00:00Z")
	f := newFakeSpecTown(clean)
	f.showSeq["gt-blind"] = []specRead{{spec: clean}, {err: errors.New("beads database unreachable")}}

	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || len(r.Dispatched) != 0 {
		t.Fatalf("slung blind on a failed re-read: slung %v report %+v", f.slung, r)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, specStaleReason) || !strings.Contains(r.Skipped[0].Line, "re-read failed") {
		t.Fatalf("skipped = %+v, want the failed re-read named", r.Skipped)
	}
	if len(r.Errors) != 0 || len(f.labels) != 0 {
		t.Fatalf("errors %v labels %v, want a plain skip the next tick retries", r.Errors, f.labels)
	}
}
