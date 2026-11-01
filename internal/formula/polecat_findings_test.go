package formula

import (
	"strings"
	"testing"
)

var polecatWorkFormulas = []string{
	"mol-polecat-work",
	"mol-polecat-work-monorepo",
}

// TestPolecatFormulasFileNoBeads pins gt-wguxe: a polecat reports a defect met
// outside its bead through gt done --finding, and the town files the Found bead
// in the rig that owns the code. A create in the formula files it from the
// polecat's seat instead, where a town skeleton holding only that seat's rig
// strands it. Both polecat work formulas carry the instruction, so a monorepo
// polecat reports the way a default polecat does.
func TestPolecatFormulasFileNoBeads(t *testing.T) {
	t.Parallel()
	for _, name := range polecatWorkFormulas {
		t.Run(name+"/report through gt done --finding", func(t *testing.T) {
			t.Parallel()
			text := embeddedFormulaText(t, name)
			if !strings.Contains(text, "gt done --finding") {
				t.Errorf("%s carries no gt done --finding instruction", name)
			}
		})
		t.Run(name+"/run no bead create", func(t *testing.T) {
			t.Parallel()
			text := embeddedFormulaText(t, name)
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(line, "gt bead create") {
					t.Errorf("%s still runs a create; report the defect through gt done --finding: %s", name, strings.TrimSpace(line))
				}
			}
		})
	}
}

// embeddedFormulaText returns the named formula's embedded text, the text a
// polecat reads when the dispatcher renders the formula.
func embeddedFormulaText(t *testing.T, name string) string {
	t.Helper()
	raw, err := GetEmbeddedFormulaContent(name)
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(%s): %v", name, err)
	}
	return string(raw)
}
