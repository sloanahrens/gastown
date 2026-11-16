package formula

import (
	"strings"
	"testing"
)

// A sandboxed polecat once read a refused shell connection to port 3307 as a
// Dolt outage, escalated HIGH and parked (gt-0chpv). F7 (gt-fgnho3) removes the
// probe rather than fixing its wording: the load-context step reads the
// assignment with `bd show`, and no step sends a seat to a Dolt health check.
func TestNoBeadAccessPolecatReadsTheAssignmentWithBdShowOnly(t *testing.T) {
	t.Parallel()
	text := strings.ToLower(embeddedFormulaText(t, "mol-polecat-work"))
	if !strings.Contains(text, "bd show {{issue}}") {
		t.Fatal("mol-polecat-work names no `bd show {{issue}}`; a seat with bead access has no way to read its assignment")
	}
	for _, banned := range []string{
		"gt dolt status",
		"bd list --limit 1",
		"port 3307",
		"restart dolt",
		"dolt health",
	} {
		if i := strings.Index(text, banned); i >= 0 {
			t.Errorf("mol-polecat-work names %q at byte %d; the Dolt probe is gone and no step should name a Dolt health check", banned, i)
		}
	}
}
