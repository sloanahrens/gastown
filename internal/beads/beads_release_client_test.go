package beads_test

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// These run the force re-sling's guarded hand-back over beadsfake, the way any
// Client runs it now that TransferIfUnassigned is a free function over
// beads.Client (gt-ct2ob). The export is what puts them in the external test
// package: importing the fake from package beads would be an import cycle.

// TestTransferIfUnassignedOverClient covers the Client path's compare: a bead
// still open and unassigned is handed back, and one another actor has taken,
// or a closed one, is left alone.
func TestTransferIfUnassignedOverClient(t *testing.T) {
	t.Parallel()
	const holder = "gastown/polecats/Nux"
	hooked := beads.StatusHooked

	t.Run("open and unassigned is restored", func(t *testing.T) {
		t.Parallel()
		c := beadsfake.New()
		is, err := c.Create(beads.CreateOptions{Title: "unhooked work", Priority: -1})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		transferred, err := beads.TransferIfUnassigned(c, is.ID, hooked, holder)
		if err != nil || !transferred {
			t.Fatalf("TransferIfUnassigned = %v, %v; want true, nil", transferred, err)
		}
		if got, _ := c.Show(is.ID); got.Status != hooked || got.Assignee != holder {
			t.Errorf("bead ended %s to %q, want %s to %s", got.Status, got.Assignee, hooked, holder)
		}
	})

	t.Run("a bead another actor hooked is left with them", func(t *testing.T) {
		t.Parallel()
		thief := "gastown/polecats/Toast"
		c := beadsfake.New()
		is, err := c.Create(beads.CreateOptions{Title: "taken work", Priority: -1})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := c.Update(is.ID, beads.UpdateOptions{Status: &hooked, Assignee: &thief}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		transferred, err := beads.TransferIfUnassigned(c, is.ID, hooked, holder)
		if err != nil || transferred {
			t.Errorf("TransferIfUnassigned over a hooked bead = %v, %v; want false, nil", transferred, err)
		}
		if got, _ := c.Show(is.ID); got.Status != hooked || got.Assignee != thief {
			t.Errorf("bead ended %s to %q, want %s's hold kept", got.Status, got.Assignee, thief)
		}
	})

	t.Run("a closed bead is not reopened", func(t *testing.T) {
		t.Parallel()
		c := beadsfake.New()
		is, err := c.Create(beads.CreateOptions{Title: "closed work", Priority: -1})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := c.Close(is.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		transferred, err := beads.TransferIfUnassigned(c, is.ID, hooked, holder)
		if err != nil || transferred {
			t.Errorf("TransferIfUnassigned over a closed bead = %v, %v; want false, nil", transferred, err)
		}
		if got, _ := c.Show(is.ID); got.Status != string(beads.StatusClosed) || got.Assignee != "" {
			t.Errorf("bead ended %s to %q, want it left closed and unassigned", got.Status, got.Assignee)
		}
	})

	t.Run("a missing bead is an error", func(t *testing.T) {
		t.Parallel()
		transferred, err := beads.TransferIfUnassigned(beadsfake.New(), "gt-nosuch", hooked, holder)
		if transferred || !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("TransferIfUnassigned(missing) = %v, %v; want false, ErrNotFound", transferred, err)
		}
	})
}
