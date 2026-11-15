package taskmap

import (
	"strings"
	"testing"
)

// TestMolDagCheckNamesPrintedMarks pins the mol dag Check to the marks
// internal/cmd/molecule_dag.go prints on each step line (gt-htrpjh): a step
// carries one glyph, and the words done, in_progress, ready and blocked appear
// only once, in the legend.
func TestMolDagCheckNamesPrintedMarks(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("mol dag")
	if !ok {
		t.Fatal(`Lookup("mol dag") found no entry`)
	}
	for _, printed := range []string{"✓", "⧖", "○", "◌"} {
		if !strings.Contains(e.Check, printed) {
			t.Errorf("mol dag Check = %q, want it to name the printed step mark %q", e.Check, printed)
		}
	}
}
