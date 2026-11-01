package land

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// A rework round is reviewed against the refusal that sent it back: the landing
// passes the previous round's findings to om, which classifies each as resolved,
// unresolved or regressed instead of raising a fresh set of minors about a diff
// that has not changed (gt-cz07b).

// TestOMReviewerPassesThePreviousRoundsFindings: a review that has them names
// --prior-findings, and the file it points at is the JSON array of findings om
// classifies (gt-cz07b).
func TestOMReviewerPassesThePreviousRoundsFindings(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	prior := []Finding{
		{ID: "f-1a2b", Severity: "minor", Path: "internal/land/review.go", Line: 317, Title: "the argv is built in one long statement"},
		{ID: "f-3c4d", Severity: "major", Path: "internal/land/land.go", Line: 940, Title: "the retry runs on a canceled context"},
	}
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-abc"}
	r := omStubbed(t, bin, stub)
	if _, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{}, Dir: "/worktree", Base: "aaa", Head: "bbb", BeadID: "gt-abc", PriorFindings: prior}); err != nil {
		t.Fatalf("ReviewBead: %v", err)
	}
	path := flagValue(t, stub.argv, "--prior-findings")
	if path == "" {
		t.Fatalf("argv = %v; want --prior-findings <file>", stub.argv)
	}
	var got []Finding
	if err := json.Unmarshal([]byte(stub.prior), &got); err != nil {
		t.Fatalf("the file om read is not a JSON array of findings (%v): %s", err, stub.prior)
	}
	if !reflect.DeepEqual(got, prior) {
		t.Errorf("om read %+v, want %+v", got, prior)
	}
	// --prior-head names the last APPROVED review, and this round was refused:
	// naming it would tell om the diff already passed (gt-cz07b).
	if head := flagValue(t, stub.argv, "--prior-head"); head != "" {
		t.Errorf("argv carries --prior-head %s on a round that was rejected", head)
	}
}

// TestOMReviewerOmitsPriorFindingsWithoutThem: a first review carries no
// --prior-findings, so om's round-1 behavior is untouched (gt-cz07b).
func TestOMReviewerOmitsPriorFindingsWithoutThem(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-abc"}
	r := omStubbed(t, bin, stub)
	if _, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{}, Dir: "/worktree", Base: "aaa", Head: "bbb", BeadID: "gt-abc"}); err != nil {
		t.Fatalf("ReviewBead: %v", err)
	}
	if got := flagValue(t, stub.argv, "--prior-findings"); got != "" {
		t.Errorf("argv = %v; a review with no previous round carries no --prior-findings", stub.argv)
	}
}

// TestLandPassesThePreviousRoundsFindingsOnAReworkRound: the review of a
// rejected landing carries the refusal's findings, and the first review of a
// bead carries none (gt-cz07b).
func TestLandPassesThePreviousRoundsFindingsOnAReworkRound(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	rev := &sequenceReviewer{t: t, steps: []reviewStep{
		{verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.74, Findings: []Finding{
			{ID: "f-9z8y", Severity: "minor", Path: "internal/land/land.go", Line: 940, Title: "the retry runs on a canceled context", Detail: "long prose om wrote"},
			{ID: "f-7x6w", Severity: "minor", Path: "internal/land/note.go", Line: 12, Title: "the marker is spelled out twice"},
		}}},
		{verdict: Verdict{Verdict: VerdictApprove, Score: 0.88}},
	}}
	l := f.lander()
	l.Reviewer = rev

	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first landing passed; want a review rejection")
	}
	if got := rev.inputs[0].PriorFindings; len(got) != 0 {
		t.Errorf("the first review carried %+v; want no prior findings on attempt 1", got)
	}

	// The author resubmits: gt done puts the ready label back on the head the
	// landing refused.
	if err := f.bd.Update(f.work.BeadID, beads.UpdateOptions{AddLabels: []string{LabelReadyToLand}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("the rework round: %v", err)
	}
	if len(rev.inputs) != 2 {
		t.Fatalf("reviewer calls = %d, want 2", len(rev.inputs))
	}
	// The refusal note is the record these come from, and it carries what om
	// matches a prior finding on: the id, with path and title behind it.
	want := []Finding{
		{ID: "f-9z8y", Severity: "minor", Path: "internal/land/land.go", Line: 940, Title: "the retry runs on a canceled context"},
		{ID: "f-7x6w", Severity: "minor", Path: "internal/land/note.go", Line: 12, Title: "the marker is spelled out twice"},
	}
	if got := rev.inputs[1].PriorFindings; !reflect.DeepEqual(got, want) {
		t.Errorf("the rework round carried %+v, want the previous round's findings %+v", got, want)
	}
}

// TestPriorReviewFindings: the findings of the last refusal, or nothing when no
// refusal raised any (gt-cz07b).
func TestPriorReviewFindings(t *testing.T) {
	t.Parallel()
	review := FormatRejectionNote(RejectionNote{
		Attempt: 1, Kind: string(RejectReview), Reason: "om requested changes (score 0.74, 1 finding(s))",
		Branch: "polecat/opal/gt-abc+x1", Target: "main", MR: "gt-abc",
		Findings: []Finding{{ID: "f-9z8y", Severity: "minor", Path: "a.go", Line: 3, Title: "unclear"}},
	})
	gate := FormatRejectionNote(RejectionNote{
		Attempt: 1, Kind: string(RejectGate), Reason: "the candidate gate make gate failed on the merged tree",
		Branch: "polecat/opal/gt-abc+x1", Target: "main", MR: "gt-abc",
	})
	for _, tc := range []struct {
		name  string
		notes string
		want  []Finding
	}{
		{name: "no refusal on the bead", notes: FormatReadyNote(Work{BeadID: "gt-abc", Branch: "b", Head: "h", Target: "main"})},
		{name: "a review refusal", notes: review, want: []Finding{{ID: "f-9z8y", Severity: "minor", Path: "a.go", Line: 3, Title: "unclear"}}},
		{name: "a gate refusal, which raised no findings", notes: gate},
		{
			// The gate refusal carries no verdict, so the review before it is
			// still the round that sent the work back (gt-g8otg).
			name:  "a review refusal, then a gate refusal",
			notes: review + "\n" + FormatRejectionNote(RejectionNote{Attempt: 2, Kind: string(RejectGate), Reason: "gate failed", MR: "gt-abc"}),
			want:  []Finding{{ID: "f-9z8y", Severity: "minor", Path: "a.go", Line: 3, Title: "unclear"}},
		},
		{
			// A conflict and a policy refusal in between are the same shape: a
			// non-review refusal that carries no finding of its own.
			name: "a review refusal, then a conflict, then a policy refusal",
			notes: review +
				"\n" + FormatRejectionNote(RejectionNote{Attempt: 1, Kind: string(RejectConflict), Reason: "the branch conflicts with main", MR: "gt-abc"}) +
				"\n" + FormatRejectionNote(RejectionNote{Attempt: 1, Kind: string(RejectPolicy), Reason: "the root .om.json changed", MR: "gt-abc"}),
			want: []Finding{{ID: "f-9z8y", Severity: "minor", Path: "a.go", Line: 3, Title: "unclear"}},
		},
		{
			// The gate refusal that followed an approve carries that approve's
			// findings, and an approve writes no block of its own (gt-g8otg).
			name: "a gate refusal after an approve",
			notes: FormatRejectionNote(RejectionNote{
				Attempt: 1, Kind: string(RejectGate), Reason: "the candidate gate make gate failed on the merged tree",
				MR: "gt-abc", Findings: []Finding{
					{ID: "f-1a2b", Severity: "minor", Path: "b.go", Line: 4, Title: "naming"},
					{ID: "f-3c4d", Severity: "minor", Path: "c.go", Line: 9, Title: "comment"},
				},
			}),
			want: []Finding{
				{ID: "f-1a2b", Severity: "minor", Path: "b.go", Line: 4, Title: "naming"},
				{ID: "f-3c4d", Severity: "minor", Path: "c.go", Line: 9, Title: "comment"},
			},
		},
		{
			// A review that raised nothing answers for itself: the round before
			// it is answered and must not outlive it.
			name:  "a review refusal with no findings after one with them",
			notes: review + "\n" + FormatRejectionNote(RejectionNote{Attempt: 2, Kind: string(RejectReview), Reason: "om requested changes (score 0.70, 0 finding(s))", MR: "gt-abc"}),
		},
		{
			// Attempt 3 is reviewed against attempt 2's refusal, which is the
			// last block on the bead by then.
			name: "the second review refusal, not the first",
			notes: review + "\n" + FormatRejectionNote(RejectionNote{
				Attempt: 2, Kind: string(RejectReview), Reason: "om requested changes (score 0.76, 1 finding(s))",
				MR:       "gt-abc",
				Findings: []Finding{{ID: "f-5e6f", Severity: "minor", Path: "b.go", Line: 8, Title: "still unclear"}},
			}),
			want: []Finding{{ID: "f-5e6f", Severity: "minor", Path: "b.go", Line: 8, Title: "still unclear"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := priorReviewFindings(tc.notes); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("priorReviewFindings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestGateFailureAfterApproveCarriesTheApprovedFindings: a gate that fails
// after om approved leaves a refusal whose note holds the approve's findings,
// so the re-review classifies them instead of running cold on the diff om just
// approved (gt-g8otg). The gate refusal still spends no review attempt.
func TestGateFailureAfterApproveCarriesTheApprovedFindings(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	approved := []Finding{
		{ID: "f-1a2b", Severity: "minor", Path: "b.go", Line: 4, Title: "the name repeats the type"},
		{ID: "f-3c4d", Severity: "minor", Path: "c.go", Line: 9, Title: "the comment says what the code says"},
	}
	rev := &sequenceReviewer{t: t, steps: []reviewStep{
		{verdict: Verdict{Verdict: VerdictApprove, Score: 0.88, Findings: approved}},
		{verdict: Verdict{Verdict: VerdictApprove, Score: 0.88}},
	}}
	l := f.lander()
	l.Reviewer = rev
	runs := 0
	f.gate.fn = func(string) GateResult {
		runs++
		if runs == 1 {
			return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1, Tail: "--- FAIL: TestFirst\n"}}}
		}
		return GateResult{Passed: true, Steps: []StepResult{{Name: "test"}}}
	}

	var rej *Rejection
	if _, err := l.Land(context.Background(), f.work); !errors.As(err, &rej) || rej.Kind != RejectGate {
		t.Fatalf("the first landing = %v, want a gate rejection", err)
	}
	if got := CountRejections(f.bead().Notes); got != 0 {
		t.Errorf("the gate refusal spent %d review attempt(s), want 0", got)
	}
	if note, ok := ParseRejectionNote(f.bead().Notes); !ok || len(note.Findings) != 2 {
		t.Fatalf("the gate refusal carries %d finding(s), want the approve's 2:\n%s", len(note.Findings), f.bead().Notes)
	}

	// The author resubmits the unchanged head: gt done puts the ready label back.
	f.resubmit()
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("the rework round: %v", err)
	}
	if len(rev.inputs) != 2 {
		t.Fatalf("reviewer calls = %d, want 2", len(rev.inputs))
	}
	if got := rev.inputs[1].PriorFindings; !reflect.DeepEqual(got, approved) {
		t.Errorf("the second review carried %+v, want the approved findings %+v", got, approved)
	}
	// The approve was round 1 and the gate refusal spent no review attempt, so
	// the re-review is still round 1 (gt-l3zak).
	if got := rev.inputs[1].Round; got != 1 {
		t.Errorf("the second review is round %d, want 1: a gate refusal spends no review attempt", got)
	}
	if got := CountRejections(f.bead().Notes); got != 0 {
		t.Errorf("the bead carries %d review rejection(s) after the rework, want 0", got)
	}
}

// TestGateFailureAfterWaiverCarriesNoFindings: a waived head's gate refusal has
// no om verdict behind it, so its note carries no findings and the next review
// runs cold (gt-g8otg).
func TestGateFailureAfterWaiverCarriesNoFindings(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1, Tail: "--- FAIL: TestFirst\n"}}}
	}
	if err := f.bd.AppendNotes(f.work.BeadID, OverseerReviewedMarker+" "+f.work.Head); err != nil {
		t.Fatal(err)
	}
	if err := f.bd.Update(f.work.BeadID, beads.UpdateOptions{AddLabels: []string{LabelOverseerReviewed}}); err != nil {
		t.Fatal(err)
	}
	l := f.lander()
	// A waived head never reaches om, so a reviewer handed no answer fails the
	// test if it is called at all.
	l.Reviewer = &sequenceReviewer{t: t}

	var rej *Rejection
	if _, err := l.Land(context.Background(), f.work); !errors.As(err, &rej) || rej.Kind != RejectGate {
		t.Fatalf("the landing = %v, want a gate rejection", err)
	}
	note, ok := ParseRejectionNote(f.bead().Notes)
	if !ok || note.Kind != string(RejectGate) {
		t.Fatalf("the refusal note = %+v, %v; want a gate refusal", note, ok)
	}
	if len(note.Findings) != 0 {
		t.Errorf("the waived head's gate refusal carries %d finding(s), want none:\n%s", len(note.Findings), f.bead().Notes)
	}
}

// flagValue is the value argv gives flag, or "" when it carries none.
func flagValue(t *testing.T, argv []string, flag string) string {
	t.Helper()
	for i, a := range argv {
		if a != flag {
			continue
		}
		if i+1 == len(argv) {
			t.Fatalf("argv = %v; %s carries no value", argv, flag)
		}
		if strings.HasPrefix(argv[i+1], "--") {
			t.Fatalf("argv = %v; %s carries no value", argv, flag)
		}
		return argv[i+1]
	}
	return ""
}
