package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestCloseDescendantsCountsOnlyClosedSteps: when bd refuses some steps of
// a batch close (here, one assigned to another agent) and closes the rest,
// closeDescendantsImpl reports the refused steps as an error and counts only
// the steps that closed. It used to count every step it asked bd to close
// and return nil, so its callers took a stranded step for a closed one.
func TestCloseDescendantsCountsOnlyClosedSteps(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	mol, _ := bd.Create(beads.CreateOptions{Title: "mol", Priority: -1, Ephemeral: true})
	s1, _ := bd.Create(beads.CreateOptions{Title: "step 1", Parent: mol.ID, Priority: -1, Ephemeral: true})
	s2, _ := bd.Create(beads.CreateOptions{Title: "step 2", Parent: mol.ID, Priority: -1, Ephemeral: true})
	s3, _ := bd.Create(beads.CreateOptions{Title: "step 3", Parent: mol.ID, Priority: -1, Ephemeral: true})
	other := "gastown/polecats/other"
	if err := bd.Update(s2.ID, beads.UpdateOptions{Assignee: &other}); err != nil {
		t.Fatal(err)
	}

	n, err := closeDescendantsImpl(bd, mol.ID, false)
	if !errors.Is(err, beads.ErrCloseRefused) {
		t.Errorf("closeDescendantsImpl error = %v, want it to report the refused step", err)
	}
	var pe *beads.PartialCloseError
	if !errors.As(err, &pe) || len(pe.NotClosed) != 1 || pe.NotClosed[0] != s2.ID {
		t.Errorf("error %v does not name %s as not closed", err, s2.ID)
	}
	if n != 2 {
		t.Errorf("closed count = %d, want 2 (%s and %s; %s was refused)", n, s1.ID, s3.ID, s2.ID)
	}
	if got, _ := bd.Show(s2.ID); got.Status != "open" {
		t.Errorf("refused step %s status %q, want open", s2.ID, got.Status)
	}

	// Forced, every step closes and counts.
	n, err = closeDescendantsImpl(bd, mol.ID, true)
	if err != nil || n != 1 {
		t.Errorf("forced closeDescendantsImpl = %d, %v; want 1 (the refused step), nil", n, err)
	}
}
