package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// reassignmentFixture is a town whose routes send gt-zd7c to the gastown rig,
// and one fake database holding gt-zd7c that answers every store.
func reassignmentFixture(t *testing.T) (townRoot string, db *beadsfake.Fake) {
	t.Helper()
	townRoot = t.TempDir()
	writeGastownRoutes(t, townRoot)
	db = beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-zd7c", Title: "t", Status: "hooked"})
	return townRoot, db
}

// commentText is every comment on id, joined.
func commentText(t *testing.T, db *beadsfake.Fake, id string) string {
	t.Helper()
	comments, err := db.Comments(id)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, c := range comments {
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, "\n")
}

// TestRecordReassignmentCarriesOutgoingAssignee is the gt-zd7c fix: the bead
// itself must name the polecat whose assignee the sling overwrote, or an audit
// that starts from the bead has no way back to the earlier branch. Branch
// enumeration that cannot tell still writes the record.
func TestRecordReassignmentCarriesOutgoingAssignee(t *testing.T) {
	t.Parallel()
	townRoot, db := reassignmentFixture(t)
	var branchesFor string
	branches := func(tr, beadID string) []string {
		branchesFor = beadID
		if tr != townRoot {
			t.Errorf("branches asked of town %q, want %q", tr, townRoot)
		}
		return nil
	}

	fakeSlingStores(db).recordReassignment(branches, io.Discard, townRoot, "gt-zd7c", "gastown/polecats/jasper", "gastown/polecats/obsidian", "mayor")

	got := commentText(t, db, "gt-zd7c")
	for _, want := range []string{
		"REASSIGNED: gastown/polecats/jasper -> gastown/polecats/obsidian",
		"Branch: (unknown)",
		"By: mayor",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reassignment record missing %q; got: %q", want, got)
		}
	}
	if branchesFor != "gt-zd7c" {
		t.Errorf("surviving branches listed for %q, want gt-zd7c", branchesFor)
	}
}

// TestRecordReassignmentSkipsNoOps guards the write budget: Dolt takes a
// permanent commit per comment, and neither an unassigned bead nor an
// unchanged assignee has anything to record.
func TestRecordReassignmentSkipsNoOps(t *testing.T) {
	t.Parallel()
	townRoot, db := reassignmentFixture(t)
	branches := func(string, string) []string {
		t.Error("a no-op reassignment must not list branches")
		return nil
	}

	stores := fakeSlingStores(db)
	stores.recordReassignment(branches, io.Discard, townRoot, "gt-zd7c", "", "gastown/polecats/obsidian", "mayor")
	stores.recordReassignment(branches, io.Discard, townRoot, "gt-zd7c", "gastown/polecats/obsidian", "gastown/polecats/obsidian", "mayor")

	if got := commentText(t, db, "gt-zd7c"); got != "" {
		t.Errorf("expected no comment for a no-op reassignment, got: %q", got)
	}
}
