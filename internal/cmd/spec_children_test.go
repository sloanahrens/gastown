package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// slingStores.children is bd's parent-child view, so a leaf bead that has a
// parent reports no children: the raw dependency scan would answer it with its
// PARENT, and the container rule would then hold every subtask (gt-gektq).
func TestSlingStoresChildrenReadsOnlyRealChildren(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	parent := mustBead(t, db, beads.CreateOptions{Title: "parent"})
	open := mustBead(t, db, beads.CreateOptions{Title: "child open", Parent: parent.ID})
	closed := mustBead(t, db, beads.CreateOptions{Title: "child closed", Parent: parent.ID})
	leaf := mustBead(t, db, beads.CreateOptions{Title: "subtask", Parent: parent.ID})
	if err := db.Close(closed.ID); err != nil {
		t.Fatalf("close %s: %v", closed.ID, err)
	}
	stores := fakeSlingStores(db)

	kids, err := stores.children("", parent.ID)
	if err != nil {
		t.Fatalf("children(%s): %v", parent.ID, err)
	}
	got := map[string]string{}
	for _, k := range kids {
		got[k.ID] = k.Status
	}
	if len(got) != 3 || got[open.ID] != "open" || got[closed.ID] != "closed" || got[leaf.ID] != "open" {
		t.Fatalf("children(%s) = %+v, want all three with their statuses", parent.ID, got)
	}
	if none, err := stores.children("", leaf.ID); err != nil || len(none) != 0 {
		t.Fatalf("children(leaf) = %v, %v; want none — the leaf's parent is not its child", none, err)
	}
}

// mustBead creates a bead or fails the test.
func mustBead(t *testing.T, db *beadsfake.Fake, opts beads.CreateOptions) *beads.Issue {
	t.Helper()
	issue, err := db.Create(opts)
	if err != nil {
		t.Fatalf("create %q: %v", opts.Title, err)
	}
	return issue
}
