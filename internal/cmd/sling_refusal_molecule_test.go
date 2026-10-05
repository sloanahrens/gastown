package cmd

import (
	"strings"
	"testing"
)

// TestSlingRefusalNamesTheMolecules: when the safe-to-burn gate holds a bead
// back (sling_helpers.go isOrphanMolecule), the refusal is the only record the
// daemon's dispatch leaves behind, so it names the molecule ids. Without them
// a molecule left attached by a refused re-sling cannot be found from the log
// (gt-mddzp).
func TestSlingRefusalNamesTheMolecules(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	// Unassigned and blocked: the gate refuses rather than auto-burning.
	h.addBead(slingBead, beadInfo{Status: "blocked"})
	h.molecules[slingBead] = []string{"gt-wisp-old", "gt-wisp-older"}

	err := h.sling(slingBead, "gastown")
	if err == nil {
		t.Fatal("expected the safe-to-burn gate to refuse the re-sling, got nil")
	}
	for _, id := range []string{"gt-wisp-old", "gt-wisp-older"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("refusal %q does not name the molecule %s", err, id)
		}
	}
}
