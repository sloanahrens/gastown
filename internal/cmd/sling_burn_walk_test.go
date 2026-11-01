package cmd

import (
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// burnFixture pours n molecules of steps each under a work bead and returns
// the bead and the molecules, in the order a sling would burn them.
func burnFixture(t *testing.T, count, steps int) (*beadsfake.Fake, *beads.Issue, []string) {
	t.Helper()
	db := beadsfake.New()
	bead := mustCreateBead(t, db, beads.CreateOptions{Title: "re-slung work"})
	molecules := make([]string, 0, count)
	for i := 0; i < count; i++ {
		mol := mustCreateBead(t, db, beads.CreateOptions{Title: fmt.Sprintf("molecule %d", i)})
		molecules = append(molecules, mol.ID)
		for j := 0; j < steps; j++ {
			childOf(t, db, fmt.Sprintf("step %d.%d", i, j), mol.ID)
		}
	}
	return db, bead, molecules
}

// TestBurnMoleculesWalksEachMoleculeOnce: a burn reads each molecule's tree
// once, in its step 1 walk, instead of walking it a second time in step 4;
// only a molecule whose first walk failed is walked again (gt-p9m39).
func TestBurnMoleculesWalksEachMoleculeOnce(t *testing.T) {
	t.Parallel()

	t.Run("clean walks are not repeated", func(t *testing.T) {
		t.Parallel()
		db, bead, molecules := burnFixture(t, 3, 12)
		counter := &descendantsReadCounter{Client: db}
		stores := slingStores{routed: func(string) slingStore { return counter }}

		if err := stores.burnMolecules(molecules, bead.ID, t.TempDir()); err != nil {
			t.Fatalf("burnMolecules: %v", err)
		}
		if got := counter.total(); got > 6 {
			t.Errorf("burning 3 molecules of 12 steps made %d child reads (%d Children, %d ChildrenOf); want at most 6, two per molecule",
				got, counter.children, counter.childrenOf)
		}
		for _, mol := range molecules {
			if got := counter.readsOf(mol); got != 1 {
				t.Errorf("molecule %s read %d times; want once", mol, got)
			}
		}
	})

	t.Run("a failed walk is retried once", func(t *testing.T) {
		t.Parallel()
		db, bead, molecules := burnFixture(t, 3, 12)
		failed := molecules[1]
		counter := &descendantsReadCounter{Client: db, refuse: map[string]bool{failed: true}}
		stores := slingStores{routed: func(string) slingStore { return counter }}

		if err := stores.burnMolecules(molecules, bead.ID, t.TempDir()); err != nil {
			t.Fatalf("burnMolecules: %v", err)
		}
		if got := counter.readsOf(failed); got != 2 {
			t.Errorf("the molecule whose first walk failed was read %d times; want 2, its step 1 walk and the step 4 retry", got)
		}
		for _, mol := range molecules {
			if mol == failed {
				continue
			}
			if got := counter.readsOf(mol); got != 1 {
				t.Errorf("molecule %s read %d times; want once", mol, got)
			}
		}
	})
}
