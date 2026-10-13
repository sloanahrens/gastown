package done

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// writeCountingClient counts the bd writes made on the work bead; each is a
// Dolt commit.
type writeCountingClient struct {
	beads.Client
	writes []string
}

func (c *writeCountingClient) AppendNotes(id, note string) error {
	c.writes = append(c.writes, "notes")
	return c.Client.AppendNotes(id, note)
}

func (c *writeCountingClient) Update(id string, opts beads.UpdateOptions) error {
	c.writes = append(c.writes, "update")
	return c.Client.Update(id, opts)
}

// TestMarkReadyToLandIsOneBdWrite: the READY TO LAND block and the labels go
// out in one update, one Dolt commit instead of two, and the bead still reads
// back as ready with the block, a stale rework label gone.
func TestMarkReadyToLandIsOneBdWrite(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New(beadsfake.WithPrefix("bd"))
	fake.Seed(beads.Issue{ID: "bd-source", Title: "the work", Type: "task", Status: string(beads.StatusHooked),
		Labels: []string{land.LabelRework, land.LabelOverseerReviewed}})
	bd := &writeCountingClient{Client: fake}
	work := land.Work{BeadID: "bd-source", Branch: "polecat/refuge/bd-source", Head: strings.Repeat("a", 40), Target: "main"}

	if err := markReadyToLand(bd, work); err != nil {
		t.Fatalf("markReadyToLand: %v", err)
	}
	if len(bd.writes) != 1 || bd.writes[0] != "update" {
		t.Errorf("bd writes = %v, want one update", bd.writes)
	}
	issue, err := fake.Show("bd-source")
	if err != nil {
		t.Fatal(err)
	}
	if !beads.HasLabel(issue, land.LabelReadyToLand) || beads.HasLabel(issue, land.LabelRework) || beads.HasLabel(issue, land.LabelOverseerReviewed) {
		t.Errorf("labels = %v, want %s alone", issue.Labels, land.LabelReadyToLand)
	}
	got, ok := land.ParseReadyNote(issue.Notes)
	if !ok || got.Branch != work.Branch || got.Head != work.Head || got.Target != work.Target || got.Submitted.IsZero() {
		t.Errorf("READY TO LAND block = %+v (%v) from notes %q", got, ok, issue.Notes)
	}
}
