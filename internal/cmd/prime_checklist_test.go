package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// cookedPolecatTree is bd's cooked tree for an attached mol-polecat-work.
const cookedPolecatTree = `{"formula": "mol-polecat-work", "type": "workflow", "steps": [
  {"id": "load", "title": "COOKED Load", "description": "COOKED body load", "children": []},
  {"id": "submit", "title": "COOKED Submit", "description": "COOKED body submit", "children": []}
]}`

// An attachment is rendered from its formula as bd cooks it, whether or not
// the dispatch poured step beads for it: the formula text is the one source
// (the step wisp removal, gt-nzpmju).
func TestPrimeChecklist_RendersTheCookedFormulaNotPouredSteps(t *testing.T) {
	t.Parallel()
	cook := &fakeCook{out: cookTree(cookedPolecatTree)}
	ctx := RoleContext{Role: RolePolecat, formulaOpen: cook.open}
	att := &beads.AttachmentFields{AttachedFormula: "mol-polecat-work", AttachedMolecule: "gt-wisp-live"}

	var buf bytes.Buffer
	ctx.showChecklist(&buf, att)
	out := buf.String()
	for _, want := range []string{"(2 steps from mol-polecat-work)", "### Step 1: COOKED Load", "COOKED body load",
		"### Step 2: COOKED Submit", "Only step 1 is shown in full"} {
		if !strings.Contains(out, want) {
			t.Errorf("checklist missing %q:\n%s", want, out)
		}
	}

	step, err := ctx.primeStepChecklist(&beads.Issue{ID: "gt-1",
		Description: "attached_molecule: gt-wisp-live\nattached_formula: mol-polecat-work\n"}, "mol-polecat-work")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := renderFormulaStep(step.name, step.steps, 2); err != nil || !strings.Contains(body, "COOKED body submit") {
		t.Errorf("gt prime --step 2 = %q, %v; want the cooked step", body, err)
	}
	prompt, err := renderRalphLoopPrompt(ctx, att)
	if err != nil || !strings.Contains(prompt, "COOKED body load") || !strings.Contains(prompt, "COOKED body submit") {
		t.Errorf("Ralph prompt = %v; want every cooked step:\n%s", err, prompt)
	}
	if n := len(cook.called("cook mol-polecat-work")); n == 0 {
		t.Errorf("prime did not cook the formula")
	}
}

// An attachment that names no formula has nothing to cook, so prime refuses it
// rather than guessing at steps.
func TestAttachmentChecklist_RefusesAnAttachmentWithNoFormula(t *testing.T) {
	t.Parallel()
	cook := &fakeCook{out: cookTree(cookedPolecatTree)}
	ctx := RoleContext{TownRoot: t.TempDir(), formulaOpen: cook.open}
	_, err := ctx.attachmentChecklist(&beads.AttachmentFields{AttachedMolecule: "gt-wisp-live"})
	if err == nil || !strings.Contains(err.Error(), "no formula") {
		t.Fatalf("attachmentChecklist = %v; want a refusal naming the missing formula", err)
	}
	if n := len(cook.called("cook ")); n != 0 {
		t.Fatalf("cooked %d formulas with none named", n)
	}
}

// showChecklist renders no checklist for that attachment: one warning line goes
// to stderr and nothing else.
func TestShowChecklist_NoFormulaRendersNothing(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	RoleContext{TownRoot: t.TempDir()}.showChecklist(&buf, &beads.AttachmentFields{AttachedMolecule: "gt-wisp-live"})
	if buf.Len() != 0 {
		t.Fatalf("no-formula attachment rendered %q", buf.String())
	}
}
