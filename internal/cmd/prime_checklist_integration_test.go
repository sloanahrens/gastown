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

// TestIntegrationPrimeChecklist_FormulaEditedAfterPour is gt-fd2cu.2 on real
// bd (gt-jxiks): a molecule poured by bd, whose formula is then rewritten on
// disk, still renders its poured steps in prime's checklist, gt prime --step
// and the Ralph prompt. The cook of the edited file is the control: bd does
// see the edit, prime just never reads it for a poured molecule.
func TestIntegrationPrimeChecklist_FormulaEditedAfterPour(t *testing.T) {
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

	ctx := RoleContext{Role: RolePolecat, TownRoot: dir, WorkDir: dir, molecules: b}
	cooked, err := ctx.formulaCooker().cookForRender("mol-itest", dir, "", nil)
	if err != nil || len(cooked.Steps) != 1 || cooked.Steps[0].Title != "EDITED STEP" {
		t.Fatalf("control: bd cook of the edited formula = %+v, %v; want the edited step", cooked, err)
	}

	att := &beads.AttachmentFields{AttachedFormula: "mol-itest", AttachedMolecule: mol}
	c, err := ctx.attachmentChecklist(att)
	if err != nil || len(c.steps) != 3 {
		t.Fatalf("poured checklist = %+v, %v; want 3 steps", c.steps, err)
	}
	if err := b.Close(c.steps[0].ID); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	ctx.showChecklist(&buf, att)
	got := buf.String()
	for _, want := range []string{"(3 steps from mol-itest)", "### Step 1: Load context (done)", "### Step 2: Implement\n\nPOURED BODY implement", "### Step 3: Submit"} {
		if !strings.Contains(got, want) {
			t.Errorf("checklist missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "EDITED") {
		t.Errorf("checklist shows the edited formula:\n%s", got)
	}

	step, err := ctx.primeStepChecklist(&beads.Issue{ID: "gt-itest", Description: "attached_molecule: " + mol + "\nattached_formula: mol-itest\n"}, "mol-itest")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := renderFormulaStep(step.name, step.steps, 3); err != nil || !strings.Contains(body, "POURED BODY submit") {
		t.Errorf("gt prime --step 3 = %q, %v; want the poured step", body, err)
	}
	prompt, err := renderRalphLoopPrompt(ctx, att)
	if err != nil || !strings.Contains(prompt, "POURED BODY load") || strings.Contains(prompt, "EDITED") {
		t.Errorf("Ralph prompt = %v, want every poured step:\n%s", err, prompt)
	}
}
