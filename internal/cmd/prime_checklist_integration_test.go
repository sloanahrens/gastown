//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

const pouredFormula = `formula = "mol-itest"
type = "workflow"
version = 1

[[steps]]
id = "load"
title = "Load context"
description = "POURED BODY load"

[[steps]]
id = "implement"
title = "Implement"
description = "POURED BODY implement"
needs = ["load"]

[[steps]]
id = "submit"
title = "Submit"
description = "POURED BODY submit"
needs = ["implement"]
`

const editedFormula = `formula = "mol-itest"
type = "workflow"
version = 2

[[steps]]
id = "edited"
title = "EDITED STEP"
description = "EDITED BODY"
`

// TestIntegrationPrimeChecklist_RendersTheCookedFormula is gt-nzpmju on real
// bd: a molecule poured by bd whose formula is then rewritten on disk renders
// the formula as it now reads, in prime's checklist, gt prime --step and the
// Ralph prompt. Poured steps are not read, so their bodies never reach a seat.
func TestIntegrationPrimeChecklist_RendersTheCookedFormula(t *testing.T) {
	t.Parallel()
	requireBd(t)
	dir, b := setupPatrolTestDB(t)
	path := filepath.Join(dir, ".beads", "formulas", "mol-itest.formula.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(pouredFormula), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := b.Run("mol", "wisp", "mol-itest", "--json")
	if err != nil {
		t.Fatalf("bd mol wisp: %v\n%s", err, out)
	}
	mol, err := parseWispIDFromJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(editedFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{Role: RolePolecat, TownRoot: dir, WorkDir: dir}
	att := &beads.AttachmentFields{AttachedFormula: "mol-itest", AttachedMolecule: mol}
	c, err := ctx.attachmentChecklist(att)
	if err != nil || len(c.steps) != 1 || c.steps[0].Title != "EDITED STEP" {
		t.Fatalf("checklist = %+v, %v; want the edited formula's one step", c.steps, err)
	}

	var buf strings.Builder
	ctx.showChecklist(&buf, att)
	got := buf.String()
	for _, want := range []string{"### Step 1: EDITED STEP", "EDITED BODY"} {
		if !strings.Contains(got, want) {
			t.Errorf("checklist missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "POURED") {
		t.Errorf("checklist shows a poured step:\n%s", got)
	}

	step, err := ctx.primeStepChecklist(&beads.Issue{ID: "gt-itest", Description: "attached_molecule: " + mol + "\nattached_formula: mol-itest\n"}, "mol-itest")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := renderFormulaStep(step.name, step.steps, 1); err != nil || !strings.Contains(body, "EDITED BODY") {
		t.Errorf("gt prime --step 1 = %q, %v; want the cooked step", body, err)
	}
	prompt, err := renderRalphLoopPrompt(ctx, att)
	if err != nil || !strings.Contains(prompt, "EDITED BODY") || strings.Contains(prompt, "POURED") {
		t.Errorf("Ralph prompt = %v; want the cooked step only:\n%s", err, prompt)
	}
}
