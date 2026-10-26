package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// writeFormulaList prints the seam's answer in both forms (gt-p85pw.11).
func TestWriteFormulaListPrintsTheSeam(t *testing.T) {
	t.Parallel()
	f := beadsfake.New()
	f.SeedFormula("mol-widget", "workflow", "widgets, made")

	var prose strings.Builder
	if err := writeFormulaList(&prose, f, false); err != nil {
		t.Fatalf("writeFormulaList prose: %v", err)
	}
	if !strings.Contains(prose.String(), "mol-widget") {
		t.Errorf("prose = %q, want the seeded formula named", prose.String())
	}

	var payload strings.Builder
	if err := writeFormulaList(&payload, f, true); err != nil {
		t.Fatalf("writeFormulaList json: %v", err)
	}
	if !strings.HasPrefix(payload.String(), "[") || !strings.Contains(payload.String(), `"mol-widget"`) {
		t.Errorf("json = %q, want the seeded formula in the JSON payload", payload.String())
	}
}

// writeFormulaShowRaw prints what bd formula show answers, in both forms.
func TestWriteFormulaShowRawPrintsTheSeam(t *testing.T) {
	t.Parallel()
	f := beadsfake.New()
	f.SeedFormula("mol-widget", "workflow", "widgets, made")

	var prose strings.Builder
	if err := writeFormulaShowRaw(&prose, f, "mol-widget", false); err != nil {
		t.Fatalf("writeFormulaShowRaw prose: %v", err)
	}
	if !strings.Contains(prose.String(), "mol-widget") || !strings.Contains(prose.String(), "Type:") {
		t.Errorf("prose = %q, want bd's formula show prose", prose.String())
	}

	var payload strings.Builder
	if err := writeFormulaShowRaw(&payload, f, "mol-widget", true); err != nil {
		t.Fatalf("writeFormulaShowRaw json: %v", err)
	}
	if !strings.Contains(payload.String(), `"formula": "mol-widget"`) {
		t.Errorf("json = %q, want the formula in the JSON payload", payload.String())
	}
}

// A missing formula reaches the operator as the command's error, with any
// answer the seam printed still on the terminal.
func TestWriteFormulaShowRawReturnsTheSeamError(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	err := writeFormulaShowRaw(&out, beadsfake.New(), "gt-no-such-formula", false)
	if err == nil {
		t.Fatal("writeFormulaShowRaw(missing) = nil, want an error")
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q, want nothing for a missing formula", out.String())
	}

	seamErr := errors.New("bd formula show mol-x: exit status 1")
	if err := writeFormulaAnswer(&out, []byte("partial\n"), seamErr); !errors.Is(err, seamErr) {
		t.Errorf("writeFormulaAnswer error = %v, want the seam's", err)
	}
	if out.String() != "partial\n" {
		t.Errorf("writeFormulaAnswer wrote %q, want the answer before the error", out.String())
	}
}
