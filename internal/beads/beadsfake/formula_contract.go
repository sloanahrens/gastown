package beadsfake

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// contractFormulaList pins the formula seam (beads.FormulaVerbs) for every
// implementation: the list has a JSON form and a prose form, and a name no
// implementation holds errors in both show forms (gt-p85pw.11).
func contractFormulaList(t *testing.T, v beads.FormulaVerbs) {
	t.Run("list is the JSON payload", func(t *testing.T) {
		out, err := v.FormulaList()
		if err != nil {
			t.Fatalf("FormulaList: %v", err)
		}
		if !json.Valid(out) {
			t.Fatalf("FormulaList = %q, want the JSON payload", out)
		}
	})

	t.Run("list text is prose", func(t *testing.T) {
		out, err := v.FormulaListText()
		if err != nil {
			t.Fatalf("FormulaListText: %v", err)
		}
		if strings.TrimSpace(string(out)) == "" {
			t.Error("FormulaListText printed nothing, want bd's prose")
		}
		if strings.Contains(string(out), `"schema_version"`) {
			t.Errorf("FormulaListText = %q, want prose, not the machine envelope", out)
		}
	})

	t.Run("an unknown formula errors", func(t *testing.T) {
		const missing = "gt-no-such-formula"
		if out, err := v.FormulaShow(missing); err == nil {
			t.Errorf("FormulaShow(%q) = %q, want an error", missing, out)
		}
		if out, err := v.FormulaShowText(missing); err == nil {
			t.Errorf("FormulaShowText(%q) = %q, want an error", missing, out)
		}
	})
}
