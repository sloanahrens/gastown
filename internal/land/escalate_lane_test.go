package land

import (
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// cappedBlock is a block the capped refusal wrote: its reason carries the
// escalated marker, and it names head so a resubmission on a new head is not a
// repeat of it (gt-dtx3ht).
func cappedBlock(branch, head string, attempt int, kind RejectionKind) string {
	return FormatRejectionNote(RejectionNote{Attempt: attempt, Kind: string(kind),
		Reason: fmt.Sprintf("r (attempt %d of %d: %s)", attempt, MaxReworkAttempts, loopEscalatedNote),
		Branch: branch, Head: head}) + "\n"
}

// TestRejectionEscalateFlag: a refusal left for a human escalates its lane once.
// Each case runs a fresh Lander over the notes it is given, so the answer comes
// from the notes alone and survives a restart (gt-dtx3ht).
func TestRejectionEscalateFlag(t *testing.T) {
	t.Parallel()
	none := func(string) string { return "" }
	for _, tc := range []struct {
		name         string
		notes        func(head string) string
		branch       string
		rej          Rejection
		wantEscalate bool
		wantRework   bool
	}{
		{"first human refusal of a bead escalates", none, fixtureBranch,
			Rejection{Kind: RejectPolicy, Reason: "r"}, true, false},
		{"rework refusal does not escalate", none, fixtureBranch,
			Rejection{Kind: RejectGate, Rework: true, Reason: "r"}, false, true},
		{"second human refusal in a lane with an escalated block does not", func(head string) string {
			return cappedBlock(fixtureBranch, "old-head", MaxReworkAttempts, RejectReview)
		}, fixtureBranch, Rejection{Kind: RejectReview, Reason: "no verdict"}, false, false},
		{"new lane after an escalated block escalates", func(head string) string {
			return cappedBlock(fixtureBranch, "old-head", MaxReworkAttempts, RejectReview)
		}, "crew/overseer/gt-x", Rejection{Kind: RejectReview, Reason: "no verdict"}, true, false},
		{"a different kind on a never-escalated bead escalates", func(string) string {
			return laneBlocks(fixtureBranch, 2, RejectGate)
		}, fixtureBranch, Rejection{Kind: RejectPolicy, Reason: "r"}, true, false},
		{"capped refusal escalates the first time", func(string) string {
			return laneBlocks(fixtureBranch, MaxReworkAttempts-1, RejectReview)
		}, fixtureBranch, Rejection{Kind: RejectReview, Rework: true, Reason: "r"}, true, false},
		{"an interrupted capped block escalates once, not as a second", func(head string) string {
			return laneBlocks(fixtureBranch, MaxReworkAttempts-1, RejectGate) +
				cappedBlock(fixtureBranch, head, MaxReworkAttempts, RejectGate)
		}, fixtureBranch, Rejection{Kind: RejectGate, Rework: true, Reason: "r"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.reseed(f.work, func(is *beads.Issue) { is.Notes = tc.notes(f.work.Head) })
			w := f.work
			w.Branch = tc.branch
			rej := tc.rej
			if err := f.lander().reject(f.bead(), w, &rej, nil); err != &rej || rej.RecordErr != nil {
				t.Fatalf("reject = %v, record error %v; want the refusal recorded", err, rej.RecordErr)
			}
			if rej.Escalate != tc.wantEscalate || rej.Rework != tc.wantRework {
				t.Errorf("escalate %v, rework %v; want escalate %v, rework %v",
					rej.Escalate, rej.Rework, tc.wantEscalate, tc.wantRework)
			}
		})
	}
}

// TestRejectionEscalateFlagAcrossPasses: the capped refusal of one pass is the
// state the next pass reads. The overseer closes the escalation, the polecat
// resubmits on a new head, and the next capped refusal in the lane raises no
// second escalation (gt-s7suwn, gt-dtx3ht).
func TestRejectionEscalateFlagAcrossPasses(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.reseed(f.work, func(is *beads.Issue) { is.Notes = laneBlocks(fixtureBranch, MaxReworkAttempts-1, RejectReview) })
	first := &Rejection{Kind: RejectReview, Rework: true, Reason: "r"}
	if err := f.lander().reject(f.bead(), f.work, first, nil); err != first || first.RecordErr != nil {
		t.Fatalf("first reject = %v, record error %v; want the refusal recorded", err, first.RecordErr)
	}
	if !first.Escalate {
		t.Fatalf("first capped refusal did not escalate")
	}
	notes := f.bead().Notes

	w := f.work
	w.Head = "resubmitted-head"
	f.reseed(w, func(is *beads.Issue) { is.Notes = notes })
	second := &Rejection{Kind: RejectReview, Rework: true, Reason: "r"}
	if err := f.lander().reject(f.bead(), w, second, nil); err != second || second.RecordErr != nil {
		t.Fatalf("second reject = %v, record error %v; want the refusal recorded", err, second.RecordErr)
	}
	if second.Rework || second.Escalate {
		t.Errorf("second capped refusal: rework %v, escalate %v; want the loop kept escalated without a second escalation",
			second.Rework, second.Escalate)
	}
}
