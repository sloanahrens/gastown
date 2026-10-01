package cmd

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// pour materializes a molecule in f the way bd pours one: an ephemeral root
// with one step bead per title, each blocked by the step before it. The
// steps are created last-first so creation order is not checklist order.
func pour(t *testing.T, f *beadsfake.Fake, titles []string, body func(i int) string) (string, []string) {
	t.Helper()
	root, err := f.Create(beads.CreateOptions{Title: "mol-polecat-work", Priority: -1, Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(titles))
	for i := len(titles) - 1; i >= 0; i-- {
		step, err := f.Create(beads.CreateOptions{Title: titles[i], Description: body(i), Parent: root.ID, Priority: -1, Ephemeral: true})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = step.ID
	}
	for i := 1; i < len(ids); i++ {
		if err := f.AddDependency(ids[i], ids[i-1]); err != nil {
			t.Fatal(err)
		}
	}
	return root.ID, ids
}

var polecatStepTitles = []string{"Load context and verify assignment", "Set up working branch", "Implement the work",
	"Self-review", "Run the gates", "Pre-verify", "Commit", "Submit work and self-clean"}

// polecatMolecule is a poured mol-polecat-work the size of the real one (8
// steps, ~19 KB of bodies), so the prime budget tests measure a realistic
// checklist read from beads.
func polecatMolecule(t *testing.T) (beads.Client, string) {
	t.Helper()
	f := beadsfake.New()
	mol, _ := pour(t, f, polecatStepTitles, func(int) string {
		return strings.Repeat("Body line of a polecat work step with commands to run.\n", 42)
	})
	return f, mol
}

func TestMoleculeChecklist_DependencyOrderStatusesAndNesting(t *testing.T) {
	t.Parallel()
	f := beadsfake.New()
	mol, ids := pour(t, f, []string{"Load", "Implement", "Submit"}, func(i int) string { return "body " + string(rune('A'+i)) })
	sub, err := f.Create(beads.CreateOptions{Title: "Implement: tests", Parent: ids[1], Priority: -1, Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(ids[0]); err != nil {
		t.Fatal(err)
	}

	steps, err := moleculeChecklist(f, mol)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range steps {
		got = append(got, s.Title+"/"+s.Status)
	}
	want := []string{"Load/closed", "Implement/open", "Implement: tests/open", "Submit/open"}
	if !slices.Equal(got, want) {
		t.Fatalf("checklist = %v, want %v", got, want)
	}
	if steps[2].ID != sub.ID || steps[0].Description != "body A" {
		t.Errorf("steps carry the bead id and body: %+v", steps)
	}
}

// TestPrimeChecklist_FormulaEditedAfterPourDoesNotChangeChecklist is D6 Q2
// (gt-fd2cu.2): once a molecule is poured, prime renders its step beads and
// never re-cooks, so the formula as it now reads on disk (here: a cook that
// answers with edited steps) changes nothing.
func TestPrimeChecklist_FormulaEditedAfterPourDoesNotChangeChecklist(t *testing.T) {
	t.Parallel()
	f := beadsfake.New()
	mol, ids := pour(t, f, []string{"Load gt-1", "Implement", "Submit"}, func(i int) string { return "Poured body " + string(rune('A'+i)) })
	if err := f.Close(ids[0]); err != nil {
		t.Fatal(err)
	}
	edited := &fakeCook{out: cookTreeJSON(`{"formula": "mol-polecat-work", "type": "workflow", "steps": [` +
		`{"id": "new", "title": "EDITED STEP", "description": "EDITED BODY", "children": []}]}`)}
	ctx := RoleContext{Role: RolePolecat, formulaRun: edited.run, molecules: f}
	att := &beads.AttachmentFields{AttachedFormula: "mol-polecat-work", AttachedMolecule: mol}

	var buf bytes.Buffer
	ctx.showChecklist(&buf, att)
	out := buf.String()
	for _, want := range []string{"(3 steps from mol-polecat-work)", "### Step 1: Load gt-1 (done)", "### Step 2: Implement\n\nPoured body B", "### Step 3: Submit",
		"Only step 2 is shown in full"} {
		if !strings.Contains(out, want) {
			t.Errorf("checklist missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "EDITED") || strings.Contains(out, "Poured body A") {
		t.Errorf("checklist shows the edited formula or a finished step's body:\n%s", out)
	}

	step, err := ctx.primeStepChecklist(&beads.Issue{ID: "gt-1", Description: "attached_molecule: " + mol + "\nattached_formula: mol-polecat-work\n"}, "mol-polecat-work")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := renderFormulaStep(step.name, step.steps, 3); err != nil || !strings.Contains(body, "Poured body C") {
		t.Errorf("gt prime --step 3 = %q, %v; want the poured step", body, err)
	}
	prompt, err := renderRalphLoopPrompt(ctx, att)
	if err != nil || !strings.Contains(prompt, "Poured body A") || strings.Contains(prompt, "EDITED") {
		t.Errorf("Ralph prompt = %v, want every poured step:\n%s", err, prompt)
	}
	if len(edited.calls) != 0 {
		t.Errorf("prime cooked a poured molecule: %d bd calls", len(edited.calls))
	}
}

// A molecule bd cannot read is one warning line and no checklist, never a
// fall back to cooking the formula.
func TestShowChecklist_UnreadableMoleculeRendersNothing(t *testing.T) {
	t.Parallel()
	f := unreadableChildren{beadsfake.New()}
	cook := &fakeCook{out: cookTreeJSON(docAuditTree)}
	var buf bytes.Buffer
	RoleContext{formulaRun: cook.run, molecules: f}.showChecklist(&buf, &beads.AttachmentFields{AttachedFormula: "mol-doc-audit", AttachedMolecule: "gt-wisp-gone"})
	if buf.Len() != 0 || len(cook.calls) != 0 {
		t.Fatalf("unreadable molecule rendered %q after %d cooks", buf.String(), len(cook.calls))
	}
}

type unreadableChildren struct{ *beadsfake.Fake }

func (unreadableChildren) ChildrenOf(...string) (map[string][]*beads.Issue, error) {
	return nil, errors.New("bd: database unavailable")
}
