package cmd

import (
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
