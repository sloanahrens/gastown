package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
)

// submittedNote is a READY TO LAND block stamped with the submission the sling
// reads the round from.
func submittedNote(at time.Time) string {
	return land.FormatReadyNote(land.Work{Branch: reworkBranch, Head: "1111aaaa", Target: "main", Worker: "garnet", Submitted: at})
}

// pastTheLastSpawn is a submission that is newer than a spawn recorded from now
// on, the order a round has: the sling is counted at its start, the submission
// at its end. The margin is a minute because FormatReadyNote keeps second
// precision, so a submission recorded in the same second as the spawn it
// followed would read as the earlier of the two.
func pastTheLastSpawn() time.Time { return time.Now().UTC().Add(time.Minute) }

// TestSlingRespawnSubmissionTime: what dates the round a sling follows. The
// respawn limit is cleared on a dated submission, and a bead that has none —
// including one that carries the rework label and nothing else — is a failed
// start the limit still counts (gt-tatcn).
func TestSlingRespawnSubmissionTime(t *testing.T) {
	t.Parallel()
	stamped := time.Date(2026, 10, 15, 3, 4, 5, 0, time.UTC)
	comment := func(text string, at time.Time) beads.Comment {
		return beads.Comment{Text: text, CreatedAt: at.UTC().Format(time.RFC3339Nano)}
	}
	older, newer := stamped.Add(-time.Hour), stamped.Add(time.Minute)

	for _, tt := range []struct {
		name          string
		labels        []string
		notes         string
		comments      []beads.Comment
		wantSubmitted time.Time
	}{
		{
			name:          "a stamped READY TO LAND block dates the submission",
			labels:        []string{land.LabelReadyToLand},
			notes:         submittedNote(stamped),
			wantSubmitted: stamped,
		},
		{
			name:   "a block from before the stamp falls back to the newest comment",
			labels: []string{land.LabelRework},
			notes:  submittedNote(time.Time{}),
			comments: []beads.Comment{
				comment("Submitted for landing: "+reworkBranch+" @ 1111aaaa onto main (attempt 1)", older),
				comment("Submitted for landing: "+reworkBranch+" @ 2222bbbb onto main (attempt 2)", newer),
			},
			wantSubmitted: newer,
		},
		{
			name:   "the bare submission a bead with no block gets still dates it",
			labels: []string{land.LabelRework},
			comments: []beads.Comment{
				comment("Submitted for landing (attempt 1)", older),
				comment("the gate tail quotes a Submitted for landing comment", newer),
			},
			wantSubmitted: older,
		},
		{
			// The label records the refusal, not when it happened, so it cannot
			// date a round on its own: a rework bead whose polecat then dies at
			// start must keep spending its limit (gt-tatcn).
			name:   "the rework label alone dates nothing",
			labels: []string{land.LabelRework},
		},
		{
			name:  "a bead that never submitted is no round",
			notes: "## Goal\n\nA fresh spec, ready for a seat.",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := &beads.Issue{ID: reworkBead, Labels: tt.labels, Notes: tt.notes}

			if got := latestSubmissionTime(issue, tt.comments); !got.Equal(tt.wantSubmitted) {
				t.Errorf("latestSubmissionTime = %v; want %v", got, tt.wantSubmitted)
			}
		})
	}
}

// TestSlingRespawnSubmissionTimeNilBead: a bead that could not be read is no
// evidence of a submission, so nothing is cleared on its account (gt-tatcn).
func TestSlingRespawnSubmissionTimeNilBead(t *testing.T) {
	t.Parallel()
	if got := latestSubmissionTime(nil, []beads.Comment{{Text: "Submitted for landing (attempt 1)"}}); !got.IsZero() {
		t.Errorf("latestSubmissionTime(nil) = %v; want zero", got)
	}
}

// TestSlingRespawnClearsASubmittedRoundsCount: a bead at its respawn limit whose
// last round reached a submission is dispatchable again, and a bead whose
// polecat only ever died at start — with or without the rework label a refusal
// left on it — is not (gt-tatcn). Every case runs against the real counter
// state, through the guard the spawn path calls.
func TestSlingRespawnClearsASubmittedRoundsCount(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		// submission is the bead's newest submission relative to the spawns
		// recorded before the sling: nil is a bead that never submitted, and
		// one behind those spawns is the rework bead whose polecat has been
		// dying since its last refusal.
		submission func() time.Time
		labels     []string
		// want is whether the limit still refuses the bead after the sling.
		want bool
	}{
		{
			name:       "the round before this one submitted",
			submission: pastTheLastSpawn,
			labels:     []string{land.LabelRework},
			want:       false,
		},
		{
			name: "three deaths at start",
			want: true,
		},
		{
			name:       "a rework bead that keeps dying",
			submission: func() time.Time { return time.Now().UTC().Add(-time.Hour) },
			labels:     []string{land.LabelRework},
			want:       true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot, _ := rigTown(t)
			for i := 0; i < config.DefaultRecoveryMaxBeadRespawns; i++ {
				if _, err := polecat.RecordBeadRespawn(townRoot, reworkBead); err != nil {
					t.Fatalf("RecordBeadRespawn: %v", err)
				}
			}
			if !polecat.ShouldBlockRespawn(townRoot, reworkBead) {
				t.Fatal("the fixture did not reach the respawn limit")
			}

			issue := beads.Issue{ID: reworkBead, Labels: tt.labels, Notes: "## Goal\n"}
			if tt.submission != nil {
				issue.Notes += "\n" + submittedNote(tt.submission())
			}
			rig := beadsfake.New()
			rig.Seed(issue)

			clearRespawnForSubmittedRound(fakeSlingStores(rig), townRoot, reworkBead)

			if got := polecat.ShouldBlockRespawn(townRoot, reworkBead); got != tt.want {
				t.Errorf("ShouldBlockRespawn after the sling = %v; want %v", got, tt.want)
			}
		})
	}
}

// commentsUnreadable is a bead store that answers show but not comments, the
// shape a database with the issue and none of its comments has.
type commentsUnreadable struct{ slingFake }

func (commentsUnreadable) Comments(string) ([]beads.Comment, error) {
	return nil, errors.New("comments unavailable")
}

// TestSlingRespawnClearsWhenCommentsAreUnreadable: a failed comment read is
// reported and the round is still dated from the READY TO LAND block, so a
// database that serves no comments cannot put a rework bead back at its limit
// (gt-tatcn).
func TestSlingRespawnClearsWhenCommentsAreUnreadable(t *testing.T) {
	t.Parallel()
	townRoot, _ := rigTown(t)
	for i := 0; i < config.DefaultRecoveryMaxBeadRespawns; i++ {
		if _, err := polecat.RecordBeadRespawn(townRoot, reworkBead); err != nil {
			t.Fatalf("RecordBeadRespawn: %v", err)
		}
	}
	rig := beadsfake.New()
	rig.Seed(beads.Issue{
		ID:     reworkBead,
		Labels: []string{land.LabelRework},
		Notes:  "## Goal\n\n" + submittedNote(pastTheLastSpawn()),
	})

	unreadable := func(string) slingStore { return commentsUnreadable{slingFake{rig}} }
	clearRespawnForSubmittedRound(slingStores{pinned: unreadable, routed: unreadable}, townRoot, reworkBead)

	if polecat.ShouldBlockRespawn(townRoot, reworkBead) {
		t.Error("ShouldBlockRespawn after the sling = true; the block's stamp dates the round")
	}
}
