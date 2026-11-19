package land

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func laneBlocks(branch string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(FormatRejectionNote(RejectionNote{Attempt: i, Kind: string(RejectReview), Reason: "r", Branch: branch}) + "\n")
	}
	return b.String()
}

// TestLaneAttemptRefusalIsNumberedByLane: a crew takeover is attempt 1; a polecat stays capped (gt-mo4uj3).
func TestLaneAttemptRefusalIsNumberedByLane(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, branch string
		attempt      int
		rework       bool
	}{
		{"takeover on a crew lane starts a loop", "crew/overseer/gt-x", 1, true},
		{"re-dispatched polecat is still capped", "polecat/dusk/gt-x+other", MaxReworkAttempts, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.reseed(f.work, func(is *beads.Issue) {
				is.Notes += "\n" + laneBlocks("polecat/furiosa/gt-x+nonce", MaxReworkAttempts)
			})
			w := f.work
			w.Branch = tc.branch
			rej := &Rejection{Kind: RejectReview, Rework: true, Reason: "r"}
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

// TestLaneAttemptCountsEachLaneApart: refusals count within their lane and kind.
func TestLaneAttemptCountsEachLaneApart(t *testing.T) {
	t.Parallel()
	polecat := laneBlocks("polecat/furiosa/gt-x+nonce", 3)
	const review, crew, dusk = string(RejectReview), "crew/overseer/gt-x", "polecat/dusk/gt-x+other"
	for _, tc := range []struct {
		name, notes, branch, kind string
		want                      int
	}{
		{"takeover lane starts at zero", polecat, crew, review, 0},
		{"second crew refusal counts the first", polecat + laneBlocks(crew, 1), crew, review, 1},
		{"third crew refusal counts both", polecat + laneBlocks(crew, 2), crew, review, 2},
		{"no-branch block is polecat lane", laneBlocks("", 1), dusk, review, 1},
		{"two crew owners count apart", laneBlocks("crew/mayor/gt-x", 2), crew, review, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := laneAttempt(tc.notes, tc.branch, tc.kind); got != tc.want {
				t.Errorf("laneAttempt(%s on %s) = %d, want %d", tc.kind, tc.branch, got, tc.want)
			}
		})
	}
}
