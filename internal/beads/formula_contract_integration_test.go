//go:build integration

package beads_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestIntegrationFormulaContract pins the formula seam to a real bd: the same
// contract the fake passes in the unit tier runs here against *beads.Beads,
// over the sample formula written to the workspace's .beads/formulas. The
// formula verbs read files, so this test needs bd but no Dolt database
// (gt-p85pw.11).
func TestIntegrationFormulaContract(t *testing.T) {
	beadsfake.RunFormulaContract(t, func(t *testing.T) beads.FormulaVerbs {
		dir := t.TempDir()
		formulas := filepath.Join(dir, ".beads", "formulas")
		if err := os.MkdirAll(formulas, 0o755); err != nil {
			t.Fatalf("creating %s: %v", formulas, err)
		}
		path := filepath.Join(formulas, beadsfake.SampleFormulaName+".formula.toml")
		if err := os.WriteFile(path, []byte(beadsfake.SampleFormulaTOML()), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return beads.New(dir)
	})
}
