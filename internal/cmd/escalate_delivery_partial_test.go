package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestCloseDeliveryBeadsCountsOnlyClosed: when bd refuses some of an
// escalation's delivery beads and closes the rest, the count is the beads
// that closed and the error names the ones left open. It used to count
// every bead it asked bd to close, hiding delivery beads that stayed open
// as phantom escalations.
func TestCloseDeliveryBeadsCountsOnlyClosed(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	a, _ := bd.Create(beads.CreateOptions{Title: "[HIGH] a", Labels: []string{"gt:message"}, Priority: -1})
	b, _ := bd.Create(beads.CreateOptions{Title: "[HIGH] b", Labels: []string{"gt:message"}, Priority: -1})
	mayor := "mayor/"
	if err := bd.Update(b.ID, beads.UpdateOptions{Assignee: &mayor}); err != nil {
		t.Fatal(err)
	}

	n, err := closeDeliveryBeads(bd, []string{a.ID, b.ID}, "hq-kl7", "gastown/witness")
	if n != 1 {
		t.Errorf("closed count = %d, want 1 (%s; %s was refused)", n, a.ID, b.ID)
	}
	var pe *beads.PartialCloseError
	if !errors.As(err, &pe) || len(pe.NotClosed) != 1 || pe.NotClosed[0] != b.ID {
		t.Errorf("error = %v, want it to name %s as not closed", err, b.ID)
	}
}
