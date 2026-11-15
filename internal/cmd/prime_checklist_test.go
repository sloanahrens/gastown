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

// renderWorkflowSection renders the workflow section prime gives a seat, with
// bd's cook answered in memory.
func renderWorkflowSection(t *testing.T, att *beads.AttachmentFields) string {
	t.Helper()
	var buf bytes.Buffer
	ctx := RoleContext{Role: RolePolecat, Rig: "myrig", TownRoot: t.TempDir(),
		formulaOpen: (&fakeCook{out: cookTree(docAuditTree)}).open}
	if err := outputMoleculeWorkflow(&buf, ctx, att); err != nil {
		t.Fatalf("outputMoleculeWorkflow: %v", err)
	}
	return buf.String()
}

// A seat whose work bead has no poured molecule still gets the whole checklist:
// every step title comes from the attached formula (the step wisp removal,
// gt-ojbfny).
func TestOutputMoleculeWorkflow_RendersEveryStepWithoutAPouredMolecule(t *testing.T) {
	t.Parallel()
	out := renderWorkflowSection(t, &beads.AttachmentFields{AttachedFormula: "mol-doc-audit"})
	for _, want := range []string{"**Formula Checklist** (3 steps from mol-doc-audit)",
		"### Step 1: Load gt-1", "### Step 2: Audit", "### Step 3: Check links"} {
		if !strings.Contains(out, want) {
			t.Errorf("seat checklist missing %q:\n%s", want, out)
		}
	}
}

// A poured molecule does not change what a seat reads: the checklist is the
// same text with or without one.
func TestOutputMoleculeWorkflow_SameChecklistWithAPouredMolecule(t *testing.T) {
	t.Parallel()
	without := renderWorkflowSection(t, &beads.AttachmentFields{AttachedFormula: "mol-doc-audit"})
	with := renderWorkflowSection(t, &beads.AttachmentFields{AttachedFormula: "mol-doc-audit", AttachedMolecule: "gt-wisp-live"})
	i, j := strings.Index(without, "**Formula Checklist**"), strings.Index(with, "**Formula Checklist**")
	if i < 0 || j < 0 {
		t.Fatalf("no checklist rendered: without at %d, with at %d", i, j)
	}
	if without[i:] != with[j:] {
		t.Errorf("a poured molecule changed the checklist:\nwithout:\n%s\nwith:\n%s", without[i:], with[j:])
	}
}

// An attachment that names no formula leaves the seat nothing to follow, and
// prime says so instead of pointing at steps it did not render.
func TestOutputMoleculeWorkflow_NoFormulaSaysThereIsNoChecklist(t *testing.T) {
	t.Parallel()
	out := renderWorkflowSection(t, &beads.AttachmentFields{AttachedMolecule: "gt-wisp-live"})
	if !strings.Contains(out, "No formula is attached") {
		t.Errorf("seat not told there is no checklist:\n%s", out)
	}
	if strings.Contains(out, "**Formula Checklist**") || strings.Contains(out, "molecule steps") {
		t.Errorf("seat pointed at steps that were not rendered:\n%s", out)
	}
}
