//go:build integration

package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestIntegrationOpenRedispatchHoldLookup_ReadsTheTownDatabase pins that the lookup reads
// the database the town's metadata.json names. It used to open the town store
// with beadsdk.Open, which ignores metadata.json and falls back to a database
// named "beads": on the production server that created an empty "beads"
// database, and every bead read from it came back "no record", so a held bead
// and an unheld one got the same answer (gt-22hdp.21).
func TestIntegrationOpenRedispatchHoldLookup_ReadsTheTownDatabase(t *testing.T) {
	requireBd(t)
	townRoot, b := setupPatrolTestDB(t)

	free, err := b.Create(beads.CreateOptions{Title: "free to redispatch", Priority: 2})
	if err != nil {
		t.Fatalf("create unheld bead: %v", err)
	}
	held, err := b.Create(beads.CreateOptions{Title: "held for sonnet", Priority: 2, Labels: []string{"needs-sonnet"}})
	if err != nil {
		t.Fatalf("create held bead: %v", err)
	}

	holdFor, release := openRedispatchHoldLookup(townRoot)
	defer release()

	if reason := holdFor(free.ID); reason != "" {
		t.Errorf("unheld bead %s: hold %q, want none (the lookup did not read the town database)", free.ID, reason)
	}
	if reason := holdFor(held.ID); reason != "label needs-sonnet" {
		t.Errorf("held bead %s: hold %q, want %q", held.ID, reason, "label needs-sonnet")
	}
}
