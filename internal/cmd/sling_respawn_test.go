package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
)

// TestSlingRespawnReworkRound: what a bead says about the round the sling
// follows. The respawn limit is cleared on either signal, and a bead that
// carries neither is a failed start the limit still counts (gt-tatcn).
func TestSlingRespawnReworkRound(t *testing.T) {
	t.Parallel()
	stamped := time.Date(2026, 10, 15, 3, 4, 5, 0, time.UTC)
	ready := func(submitted time.Time) string {
		return land.FormatReadyNote(land.Work{Branch: reworkBranch, Head: "1111aaaa", Target: "main", Worker: "garnet", Submitted: submitted})
	}
	comment := func(text string, at time.Time) beads.Comment {
		return beads.Comment{Text: text, CreatedAt: at.UTC().Format(time.RFC3339Nano)}
	}
	older, newer := stamped.Add(-time.Hour), stamped.Add(time.Minute)

	for _, tt := range []struct {
		name          string
		labels        []string
		notes         string
		comments      []beads.Comment
		wantRework    bool
		wantSubmitted time.Time
	}{
		{
			name:       "the label alone is a refusal the bead came back from",
			labels:     []string{land.LabelRework},
			wantRework: true,
		},
		{
			name:          "a stamped READY TO LAND block dates the submission",
			labels:        []string{land.LabelReadyToLand},
			notes:         ready(stamped),
			wantSubmitted: stamped,
		},
		{
			name:   "a block from before the stamp falls back to the newest comment",
			labels: []string{land.LabelRework},
			notes:  ready(time.Time{}),
			comments: []beads.Comment{
				comment("Submitted for landing: "+reworkBranch+" @ 1111aaaa onto main (attempt 1)", older),
				comment("Submitted for landing: "+reworkBranch+" @ 2222bbbb onto main (attempt 2)", newer),
			},
			wantRework:    true,
			wantSubmitted: newer,
		},
		{
			name:   "the bare submission a bead with no block gets still dates it",
			labels: []string{land.LabelRework},
			comments: []beads.Comment{
				comment("Submitted for landing (attempt 1)", older),
				comment("the gate tail quotes a Submitted for landing comment", newer),
			},
			wantRework:    true,
			wantSubmitted: older,
		},
		{
			name:  "a bead that never submitted is no round",
			notes: "## Goal\n\nA fresh spec, ready for a seat.",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := &beads.Issue{ID: reworkBead, Labels: tt.labels, Notes: tt.notes}

			rework, submitted := reworkRoundFrom(issue, tt.comments)
			if rework != tt.wantRework {
				t.Errorf("rework = %v; want %v", rework, tt.wantRework)
			}
			if !submitted.Equal(tt.wantSubmitted) {
				t.Errorf("submitted = %v; want %v", submitted, tt.wantSubmitted)
			}
		})
	}
}

// TestSlingRespawnReworkRoundNilBead: a bead that could not be read is no
// evidence of a submission, so nothing is cleared on its account (gt-tatcn).
func TestSlingRespawnReworkRoundNilBead(t *testing.T) {
	t.Parallel()
	rework, submitted := reworkRoundFrom(nil, []beads.Comment{{Text: "Submitted for landing (attempt 1)"}})
	if rework || !submitted.IsZero() {
		t.Errorf("reworkRoundFrom(nil) = %v, %v; want false, zero", rework, submitted)
	}
}

// TestSlingRespawnClearsAReworkRoundsCount: a bead at its respawn limit that
// came back from a landing rejection is dispatchable again, and one that only
// ever died at start is not (gt-tatcn). Both halves run against the real
// counter state, through the guard the spawn path calls.
func TestSlingRespawnClearsAReworkRoundsCount(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		// rejected is the bead the landing worker sent back: the rework label,
		// and the submission the refusal followed.
		rejected bool
		want     bool
	}{
		{"rejected for rework", true, false},
		{"three deaths at start", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot, _ := rigTown(t)
			issue := beads.Issue{ID: reworkBead, Notes: "## Goal\n"}
			if tt.rejected {
				issue.Labels = []string{land.LabelRework}
				issue.Notes += "\n" + land.FormatReadyNote(land.Work{Branch: reworkBranch, Head: "1111aaaa", Target: "main", Worker: "garnet", Submitted: time.Now().UTC()})
			}
			rig := beadsfake.New()
			rig.Seed(issue)
			stores := fakeSlingStores(rig)

			for i := 0; i < config.DefaultRecoveryMaxBeadRespawns; i++ {
				if _, err := polecat.RecordBeadRespawn(townRoot, reworkBead); err != nil {
					t.Fatalf("RecordBeadRespawn: %v", err)
				}
			}
			if !polecat.ShouldBlockRespawn(townRoot, reworkBead) {
				t.Fatal("the fixture did not reach the respawn limit")
			}

			clearRespawnForReworkRound(stores, townRoot, reworkBead)

			if got := polecat.ShouldBlockRespawn(townRoot, reworkBead); got != tt.want {
				t.Errorf("ShouldBlockRespawn after the sling = %v; want %v", got, tt.want)
			}
		})
	}
}
