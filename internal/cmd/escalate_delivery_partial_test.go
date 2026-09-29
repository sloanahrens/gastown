package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestCloseDeliveryBeadsForcesPastRecipients: an escalation's delivery
// beads are assigned to their recipients, so an unforced close is refused
// for them and they linger as phantom escalations. They are gastown's own
// bookkeeping, not work: they are force-closed, and the count is the beads
// that closed.
func TestCloseDeliveryBeadsForcesPastRecipients(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	a, _ := bd.Create(beads.CreateOptions{Title: "[HIGH] a", Labels: []string{"gt:message"}, Priority: -1})
	b, _ := bd.Create(beads.CreateOptions{Title: "[HIGH] b", Labels: []string{"gt:message"}, Priority: -1})
	mayor := "mayor/"
	if err := bd.Update(b.ID, beads.UpdateOptions{Assignee: &mayor}); err != nil {
		t.Fatal(err)
	}

	n, err := closeDeliveryBeads(bd, []string{a.ID, b.ID}, "hq-kl7", "gastown/witness")
	if err != nil || n != 2 {
		t.Errorf("closeDeliveryBeads = %d, %v; want 2, nil", n, err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if got, _ := bd.Show(id); got.Status != "closed" {
			t.Errorf("delivery bead %s status %q, want closed", id, got.Status)
		}
	}
	// A missing bead fails the batch and counts nothing.
	if n, err := closeDeliveryBeads(bd, []string{a.ID, "gt-nosuch"}, "hq-kl7", "gastown/witness"); !errors.Is(err, beads.ErrNotFound) || n != 0 {
		t.Errorf("closeDeliveryBeads(missing) = %d, %v; want 0, ErrNotFound", n, err)
	}
}
