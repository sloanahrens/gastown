package land

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func laneBlocks(branch string, n int, kind RejectionKind) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(FormatRejectionNote(RejectionNote{Attempt: i, Kind: string(kind), Reason: "r", Branch: branch}) + "\n")
	}
	return b.String()
}

// TestLaneAttemptRefusalIsNumberedByLane: a crew takeover is attempt 1, for
// every kind; a polecat stays capped (gt-mo4uj3).
func TestLaneAttemptRefusalIsNumberedByLane(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, branch string
		kind         RejectionKind
		attempt      int
		rework       bool
	}{
		{"takeover on a crew lane starts a loop", "crew/overseer/gt-x", RejectReview, 1, true},
		{"re-dispatched polecat is still capped", "polecat/dusk/gt-x+other", RejectReview, MaxReworkAttempts, false},
		{"takeover gate starts its own loop", "crew/overseer/gt-x", RejectGate, 1, true},
		{"takeover policy starts its own loop", "crew/overseer/gt-x", RejectPolicy, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.reseed(f.work, func(is *beads.Issue) {
				is.Notes += "\n" + laneBlocks("polecat/furiosa/gt-x+nonce", MaxReworkAttempts, tc.kind)
			})
			w := f.work
			w.Branch = tc.branch
			rej := &Rejection{Kind: tc.kind, Rework: true, Reason: "r"}
			if err := f.lander().reject(f.bead(), w, rej, nil); err != rej || rej.RecordErr != nil {
				t.Fatalf("reject = %v, record error %v; want the refusal recorded", err, rej.RecordErr)
			}
			if got, ok := ParseRejectionNote(f.bead().Notes); !ok || got.Attempt != tc.attempt || rej.Rework != tc.rework {
				t.Errorf("refusal = attempt %d, rework %v (parsed %v); want attempt %d, rework %v",
					got.Attempt, rej.Rework, ok, tc.attempt, tc.rework)
			}
		})
	}
}

// TestLaneAttemptRepeatNumbersInTheBlocksLane: a repeat completes the block an
// interrupted pass wrote and numbers in that block's lane, not the lane of the
// branch the takeover resubmits on. The block is at the ceiling in its own
// lane, so counting the new lane's zero would reset a capped loop to attempt 0
// and rework it again instead of escalating (gt-mo4uj3).
func TestLaneAttemptRepeatNumbersInTheBlocksLane(t *testing.T) {
	t.Parallel()
	const interrupted = "crew/dunlin/gt-x"
	f := newLandFixture(t)
	f.reseed(f.work, func(is *beads.Issue) {
		is.Notes += "\n" + laneBlocks(interrupted, MaxReworkAttempts-1, RejectGate) +
			FormatRejectionNote(RejectionNote{
				Attempt: MaxReworkAttempts, Kind: string(RejectGate), Reason: "r", Branch: interrupted,
				Target: f.work.Target, MR: f.work.BeadID, Head: f.work.Head,
			})
	})
	w := f.work
	w.Branch = "crew/overseer/gt-x"
	rej := &Rejection{Kind: RejectGate, Rework: true, Reason: "r"}
	if err := f.lander().reject(f.bead(), w, rej, nil); err != rej || rej.RecordErr != nil {
		t.Fatalf("reject = %v, record error %v; want the interrupted block completed", err, rej.RecordErr)
	}
	if b := f.bead(); rej.Rework || !beads.HasLabel(b, LabelNeedsOverseer) {
		t.Errorf("repeat at the ceiling: rework=%v labels=%v; want the loop escalated", rej.Rework, b.Labels)
	}
}

// TestLaneOf: a branch names its lane, and a rename outside polecat/ and crew/
// cannot reset the cap (gt-mo4uj3).
func TestLaneOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, branch, want string }{
		{"empty is polecat", "", "polecat"},
		{"polecat branch", "polecat/dusk/gt-x+n", "polecat"},
		{"crew owner alone", "crew/overseer", "crew/overseer"},
		{"crew owner's branch", "crew/overseer/gt-x", "crew/overseer"},
		{"crew owners count apart", "crew/mayor/gt-x", "crew/mayor"},
		{"bare crew has no owner", "crew/", laneOther},
		{"another branch is the shared other lane", "adam/gt-x+n", laneOther},
		{"a second other branch shares it", "wip/fix", laneOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := laneOf(tc.branch); got != tc.want {
				t.Errorf("laneOf(%q) = %q, want %q", tc.branch, got, tc.want)
			}
		})
	}
}

// TestLaneAttemptCountsEachLaneApart: refusals count within their lane and kind.
func TestLaneAttemptCountsEachLaneApart(t *testing.T) {
	t.Parallel()
	review := RejectReview
	polecat := laneBlocks("polecat/furiosa/gt-x+nonce", 3, review)
	const crew, dusk = "crew/overseer/gt-x", "polecat/dusk/gt-x+other"
	for _, tc := range []struct {
		name, notes, branch string
		want                int
	}{
		{"takeover lane starts at zero", polecat, crew, 0},
		{"second crew refusal counts the first", polecat + laneBlocks(crew, 1, review), crew, 1},
		{"third crew refusal counts both", polecat + laneBlocks(crew, 2, review), crew, 2},
		{"no-branch block is polecat lane", laneBlocks("", 1, review), dusk, 1},
		{"two crew owners count apart", laneBlocks("crew/mayor/gt-x", 2, review), crew, 0},
		{"a renamed other branch keeps counting", laneBlocks("adam/gt-x+old", 2, review), "wip/gt-x+new", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := laneAttempt(tc.notes, tc.branch, string(review)); got != tc.want {
				t.Errorf("laneAttempt(%s on %s) = %d, want %d", review, tc.branch, got, tc.want)
			}
		})
	}
}
