package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

func threeStepFormula() *formula.Formula {
	return &formula.Formula{
		Name: "mol-test-work",
		Steps: []formula.Step{
			{ID: "one", Title: "Load {{issue}}", Description: "Body of step one for {{issue}}.\nSecond line."},
			{ID: "two", Title: "Implement", Description: "Body of step two."},
			{ID: "three", Title: "Submit", Description: "Body of step three."},
		},
	}
}

func TestRenderFormulaChecklist_TitlesForAllStepsBodyForOne(t *testing.T) {
	vars := map[string]string{"issue": "gt-abc"}
	out := renderFormulaChecklist("mol-test-work", threeStepFormula(), vars, 1)

	for _, want := range []string{
		"**Formula Checklist** (3 steps from mol-test-work)",
		"### Step 1: Load gt-abc",
		"Body of step one for gt-abc.",
		"Second line.",
		"Step 2: Implement",
		"Step 3: Submit",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("checklist missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"Body of step two.", "Body of step three."} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("checklist must not include other step bodies, found %q:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "gt prime --step <N> --formula mol-test-work") {
		t.Fatalf("checklist must tell the agent how to fetch the other step bodies:\n%s", out)
	}
}

func TestRenderFormulaChecklist_FullStepSelectsBody(t *testing.T) {
	out := renderFormulaChecklist("mol-test-work", threeStepFormula(), nil, 3)
	if !strings.Contains(out, "Body of step three.") {
		t.Fatalf("expected step 3 body:\n%s", out)
	}
	if strings.Contains(out, "Body of step one") {
		t.Fatalf("step 1 body must not render when step 3 is the full step:\n%s", out)
	}
}

func TestRenderFormulaChecklist_OutOfRangeFallsBackToStepOne(t *testing.T) {
	out := renderFormulaChecklist("mol-test-work", threeStepFormula(), nil, 9)
	if !strings.Contains(out, "Body of step one") {
		t.Fatalf("out-of-range full step must fall back to step 1:\n%s", out)
	}
}

func TestRenderFormulaChecklist_EmptyFormula(t *testing.T) {
	if out := renderFormulaChecklist("x", &formula.Formula{}, nil, 1); out != "" {
		t.Fatalf("expected empty output for a formula without steps, got %q", out)
	}
}

func TestPrimePayload_UnderBudgetRendersInOrder(t *testing.T) {
	var p primePayload
	p.add("hook", 1, false, "HOOK\n")
	p.add("memories", 5, false, "MEMORIES\n")
	p.add("footer", 9, true, "FOOTER\n")

	if got := p.render(1000); got != "HOOK\nMEMORIES\nFOOTER\n" {
		t.Fatalf("unexpected render:\n%q", got)
	}
}

func TestPrimePayload_DropsLowestPriorityFirstAndNamesIt(t *testing.T) {
	var p primePayload
	p.add("hook", 1, false, strings.Repeat("H", 40)+"\n")
	p.add("directives", 3, false, strings.Repeat("D", 40)+"\n")
	p.add("memories", 5, false, strings.Repeat("M", 40)+"\n")
	p.add("footer", 9, true, "FOOTER\n")

	got := p.render(120)
	if strings.Contains(got, "MMMM") {
		t.Fatalf("memories (lowest priority) should be dropped first:\n%s", got)
	}
	if !strings.Contains(got, "DDDD") || !strings.Contains(got, "HHHH") {
		t.Fatalf("higher-priority sections must survive:\n%s", got)
	}
	if !strings.Contains(got, "omitted to fit the hook budget: memories") {
		t.Fatalf("render must name the omitted section:\n%s", got)
	}
	if !strings.HasSuffix(got, "FOOTER\n") {
		t.Fatalf("footer must stay last:\n%s", got)
	}
}

func TestPrimePayload_KeepSectionsNeverDropped(t *testing.T) {
	var p primePayload
	p.add("hook", 1, true, strings.Repeat("H", 100)+"\n")
	p.add("memories", 5, false, strings.Repeat("M", 100)+"\n")
	p.add("footer", 9, true, strings.Repeat("F", 100)+"\n")

	got := p.render(50)
	if !strings.Contains(got, "HHHH") || !strings.Contains(got, "FFFF") {
		t.Fatalf("keep sections must render even over budget:\n%s", got)
	}
	if strings.Contains(got, "MMMM") {
		t.Fatalf("droppable section must go:\n%s", got)
	}
}

func TestPrimePayload_ZeroBudgetMeansUnlimited(t *testing.T) {
	var p primePayload
	p.add("a", 1, false, strings.Repeat("A", 5000))
	p.add("b", 2, false, strings.Repeat("B", 5000))
	if got := p.render(0); len(got) != 10000 {
		t.Fatalf("budget 0 must not drop anything, got %d chars", len(got))
	}
}

func TestPrimePayload_EmptySectionsAreSkipped(t *testing.T) {
	var p primePayload
	p.add("a", 1, false, "")
	p.add("b", 2, false, "B\n")
	if got := p.render(1000); got != "B\n" {
		t.Fatalf("empty sections must not render or count, got %q", got)
	}
}
