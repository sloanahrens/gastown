package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

func TestNewFormulaCheck(t *testing.T) {
	check := NewFormulaCheck()
	if check.Name() != "formulas" {
		t.Errorf("Name() = %q, want %q", check.Name(), "formulas")
	}
	if !check.CanFix() {
		t.Error("FormulaCheck should be fixable")
	}
}

func TestFormulaCheck_Run_AllOK(t *testing.T) {
	tmpDir := t.TempDir()

	// Provision formulas fresh
	_, err := formula.ProvisionFormulas(tmpDir)
	if err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	check := NewFormulaCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want %v", result.Status, StatusOK)
	}
}

func TestFormulaCheck_Run_Missing(t *testing.T) {
	tmpDir := t.TempDir()

	// Provision formulas
	_, err := formula.ProvisionFormulas(tmpDir)
	if err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	// Delete a formula
	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	formulaPath := filepath.Join(formulasDir, "mol-deacon-patrol.formula.toml")
	if err := os.Remove(formulaPath); err != nil {
		t.Fatal(err)
	}

	check := NewFormulaCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("Status = %v, want %v", result.Status, StatusWarning)
	}
	if result.FixHint == "" {
		t.Error("should have FixHint")
	}
}

func TestFormulaCheck_Fix(t *testing.T) {
	tmpDir := t.TempDir()

	// Provision formulas
	_, err := formula.ProvisionFormulas(tmpDir)
	if err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	// Delete a formula
	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	formulaPath := filepath.Join(formulasDir, "mol-deacon-patrol.formula.toml")
	if err := os.Remove(formulaPath); err != nil {
		t.Fatal(err)
	}

	check := NewFormulaCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	// Run fix
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}

	// Verify formula was restored
	if _, err := os.Stat(formulaPath); os.IsNotExist(err) {
		t.Error("formula should have been restored")
	}

	// Re-run check - should be OK now
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("after fix, Status = %v, want %v", result.Status, StatusOK)
	}
}

// TestFormulaCheck_Run_HandEditedIsAWarning is the regression test for gt-dt7r:
// doctor used to report a hand-edited formula as a detail under an OK status,
// so a town silently running stale formula content still had a clean bill of
// health. It must be a warning, and --fix must not be the remedy.
func TestFormulaCheck_Run_HandEditedIsAWarning(t *testing.T) {
	tmpDir := t.TempDir()

	if _, err := formula.ProvisionFormulas(tmpDir); err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	edited := "mol-refinery-patrol.formula.toml"
	editedPath := filepath.Join(formulasDir, edited)
	if err := os.WriteFile(editedPath, []byte("# hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := NewFormulaCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("Status = %v, want %v: a hand-edited formula is not a clean town", result.Status, StatusWarning)
	}
	if !strings.Contains(result.Message, "undelivered") {
		t.Errorf("Message = %q, want it to say the embedded content is undelivered", result.Message)
	}

	var found bool
	for _, d := range result.Details {
		if strings.Contains(d, edited) {
			found = true
			if !strings.Contains(d, "NOT delivered") {
				t.Errorf("detail for %s = %q, want the undelivered wording", edited, d)
			}
		}
	}
	if !found {
		t.Errorf("Details do not name %s: %v", edited, result.Details)
	}
	if strings.Contains(result.FixHint, "doctor --fix") {
		t.Errorf("FixHint = %q, want a remedy --fix cannot perform here", result.FixHint)
	}

	// --fix must leave the user's edit alone.
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}
	content, err := os.ReadFile(editedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# hand-edited\n" {
		t.Error("Fix() overwrote the hand-edited formula")
	}
}
