package cmd

import (
	"errors"
	"strings"
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

// refusedMolecule builds a molecule whose step 2 bd refuses to close (it is
// assigned to another agent) and whose other steps close.
func refusedMolecule(t *testing.T) (bd *beadsfake.Fake, mol, refused *beads.Issue) {
	t.Helper()
	bd = beadsfake.New()
	mol, _ = bd.Create(beads.CreateOptions{Title: "mol", Priority: -1, Ephemeral: true})
	bd.Create(beads.CreateOptions{Title: "step 1", Parent: mol.ID, Priority: -1, Ephemeral: true})
	refused, _ = bd.Create(beads.CreateOptions{Title: "step 2", Parent: mol.ID, Priority: -1, Ephemeral: true})
	other := "gastown/polecats/other"
	if err := bd.Update(refused.ID, beads.UpdateOptions{Assignee: &other}); err != nil {
		t.Fatal(err)
	}
	return bd, mol, refused
}

// TestDiscardDescendantsForceClosesRefusedSteps: burn and squash discard a
// molecule, so a step bd refuses is force-closed rather than left open
// under the root they close next, and the forced count is reported.
func TestDiscardDescendantsForceClosesRefusedSteps(t *testing.T) {
	t.Parallel()
	bd, mol, refused := refusedMolecule(t)

	closed, forced, err := discardDescendants(bd, mol.ID)
	if err != nil {
		t.Fatalf("discardDescendants: %v", err)
	}
	if closed != 1 || forced != 1 {
		t.Errorf("closed %d forced %d, want 1 and 1", closed, forced)
	}
	if got, _ := bd.Show(refused.ID); got.Status != "closed" {
		t.Errorf("refused step %s status %q, want force-closed", refused.ID, got.Status)
	}
}

// TestCloseStepsThenRootKeepsRootOpenOnRefusal: gt done and the patrol
// helpers claim the work complete, so when bd refuses a step the root stays
// open and the error names the step and why it is still open (gt-7lx3).
func TestCloseStepsThenRootKeepsRootOpenOnRefusal(t *testing.T) {
	t.Parallel()
	bd, mol, refused := refusedMolecule(t)

	rootClosed := false
	_, err := closeStepsThenRoot(bd, mol.ID, func() error {
		rootClosed = true
		return bd.ForceCloseWithReason("done", mol.ID)
	})
	if err == nil {
		t.Fatal("closeStepsThenRoot = nil, want an error for the refused step")
	}
	if rootClosed {
		t.Error("the root was closed with a step still open")
	}
	if got, _ := bd.Show(mol.ID); got.Status == "closed" {
		t.Errorf("root %s closed, want it left open", mol.ID)
	}
	for _, want := range []string{refused.ID, "assigned to gastown/polecats/other"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// With every step closable, the root closes.
	bd2 := beadsfake.New()
	mol2, _ := bd2.Create(beads.CreateOptions{Title: "mol", Priority: -1, Ephemeral: true})
	bd2.Create(beads.CreateOptions{Title: "step", Parent: mol2.ID, Priority: -1, Ephemeral: true})
	n, err := closeStepsThenRoot(bd2, mol2.ID, func() error { return bd2.ForceCloseWithReason("done", mol2.ID) })
	if err != nil || n != 1 {
		t.Errorf("closeStepsThenRoot(all closable) = %d, %v; want 1, nil", n, err)
	}
	if got, _ := bd2.Show(mol2.ID); got.Status != "closed" {
		t.Errorf("root status %q, want closed", got.Status)
	}
}

// chainedMolecule builds a molecule of n unassigned steps chained by blocks
// dependencies, as bd mol bond makes them, with the steps' IDs in an order
// other than the chain's: step order[i] is the i-th link.
func chainedMolecule(t *testing.T, order []int) (bd *beadsfake.Fake, mol *beads.Issue, steps []*beads.Issue) {
	t.Helper()
	bd = beadsfake.New()
	mol, _ = bd.Create(beads.CreateOptions{Title: "mol", Priority: -1, Ephemeral: true})
	for range order {
		is, _ := bd.Create(beads.CreateOptions{Title: "step", Parent: mol.ID, Priority: -1, Ephemeral: true})
		steps = append(steps, is)
	}
	for i := 1; i < len(order); i++ {
		if err := bd.AddDependency(steps[order[i]].ID, steps[order[i-1]].ID); err != nil {
			t.Fatal(err)
		}
	}
	return bd, mol, steps
}

// TestCloseStepsThenRootClosesAShuffledChain: bd closes a batch in argument
// order and refuses a step whose blocker is later in the batch, and Children
// lists steps in ID order. A chained molecule's steps must still all close,
// and its root with them; before, one batch closed only the steps whose
// blockers came first and gt done left every molecule open.
func TestCloseStepsThenRootClosesAShuffledChain(t *testing.T) {
	t.Parallel()
	bd, mol, steps := chainedMolecule(t, []int{4, 2, 0, 3, 1})

	n, err := closeStepsThenRoot(bd, mol.ID, func() error { return bd.ForceCloseWithReason("done", mol.ID) })
	if err != nil {
		t.Fatalf("closeStepsThenRoot: %v", err)
	}
	if n != len(steps) {
		t.Errorf("steps closed = %d, want %d", n, len(steps))
	}
	for _, is := range steps {
		if got, _ := bd.Show(is.ID); got.Status != "closed" {
			t.Errorf("step %s status %q, want closed", is.ID, got.Status)
		}
	}
	if got, _ := bd.Show(mol.ID); got.Status != "closed" {
		t.Errorf("root status %q, want closed", got.Status)
	}
}

// TestCloseDescendantsChainWithRefusedLinkStops: a chain whose middle step
// bd genuinely refuses (another assignee) closes the links before it, keeps
// the refused step and everything after it open, and reports them.
func TestCloseDescendantsChainWithRefusedLinkStops(t *testing.T) {
	t.Parallel()
	bd, mol, steps := chainedMolecule(t, []int{2, 0, 1})
	// Chain: steps[2] <- steps[0] <- steps[1]. Refuse steps[0].
	other := "gastown/polecats/other"
	if err := bd.Update(steps[0].ID, beads.UpdateOptions{Assignee: &other}); err != nil {
		t.Fatal(err)
	}
	n, err := closeDescendantsImpl(bd, mol.ID, false)
	if n != 1 {
		t.Errorf("closed = %d, want 1 (only %s)", n, steps[2].ID)
	}
	var pe *beads.PartialCloseError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a *PartialCloseError", err)
	}
	if got := strings.Join(pe.NotClosed, ","); !strings.Contains(got, steps[0].ID) || !strings.Contains(got, steps[1].ID) {
		t.Errorf("NotClosed = %v, want %s and %s", pe.NotClosed, steps[0].ID, steps[1].ID)
	}
	rootClosed := false
	if _, err := closeStepsThenRoot(bd, mol.ID, func() error { rootClosed = true; return nil }); err == nil || rootClosed {
		t.Errorf("closeStepsThenRoot = %v, root closed %v; want an error and the root left open", err, rootClosed)
	}
}
