package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestRosterCountsAClaimAndItsSeatOnce is gt-il6mm7's acceptance box: a
// sessionless seat hooking gt-a and a claim for gt-a on the same agent are one
// seat, not two. The sling's spawn has written the polecat's agent bead with
// the hook set before its session starts, so the same read that counts the seat
// also counts the claim; appended beside each other they read a town at its cap
// as over it and the tick dispatches nothing.
func TestRosterCountsAClaimAndItsSeatOnce(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	db := beadsfake.New()
	lister := &fakeLister{sessions: map[string]map[string]string{}}

	// The spawn has made the seat — its agent bead hooks gt-a — but its session
	// has not started, so no tmux session holds it.
	writeTownPolecatDir(t, town, "gastown", "jade")
	seedPolecatAgentBead(db, town, "gastown", "jade", "gt-a")
	db.Seed(beads.Issue{ID: "gt-a", Status: string(beads.StatusInProgress), Assignee: "gastown/polecats/jade"})
	// The sling still holds its claim for that same bead on the same agent.
	writeSeatClaimForBead(t, town, "claim-1", "deepseek-flash", "gt-a")

	batch := batchOverStore(town, db)
	seats, err := poolSeatSessionsWith(lister, town, batch.disposition, nil, seatPool("deepseek-flash", 30))
	if err != nil {
		t.Fatalf("poolSeatSessionsWith: %v", err)
	}
	if len(seats) != 1 {
		t.Fatalf("poolSeatSessionsWith read %d seats (%+v), want 1: the claim and the seat that hooks its bead are one", len(seats), seats)
	}
	if got := seats[0]; got.name != "dead/gastown/jade" || got.hookedBead != "gt-a" {
		t.Fatalf("the one seat is %+v, want the sessionless seat hooking gt-a", got)
	}
}
