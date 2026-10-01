//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

// convoyFormulas are the shipped convoy formulas gt formula run dispatches.
var convoyFormulas = []string{"code-review", "design", "mol-plan-review", "mol-prd-review"}

// TestIntegrationFormulaCook renders every shipped formula through the real bd
// cook the way prime does (gt-fd2cu.1): each one cooks with no warning, a
// workflow renders a bounded checklist, the town overlay applies, a convoy
// reads as legs plus a synthesis that renders, and a workflow step keeps its
// sling target (gt-fd2cu.1.1).
func TestIntegrationFormulaCook(t *testing.T) {
	town := t.TempDir()
	if _, err := formula.ProvisionFormulas(town); err != nil {
		t.Fatalf("provision formulas: %v", err)
	}
	overlay := "[[step-overrides]]\nstep_id = \"load-context\"\nmode = \"append\"\ndescription = \"TOWN OVERLAY LINE\"\n"
	if err := os.MkdirAll(formula.OverlayDir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formula.OverlayPath(town, "mol-polecat-work"), []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, err := filepath.Glob(filepath.Join(town, ".beads", "formulas", "*.formula.toml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("provisioned formulas: %v (%d)", err, len(paths))
	}
	vars := []string{"issue=gt-cook", "feature=Cook feature"}
	cooker := realFormulaCooker()
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".formula.toml")
		f, err := cooker.cookForRender(name, town, "", vars)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(f.Warnings) > 0 {
			t.Errorf("%s: cook warnings %v", name, f.Warnings)
		}
		if slices.Contains(convoyFormulas, name) {
			checkCookedConvoy(t, name, f)
			continue
		}
		if f.Type != "workflow" {
			continue
		}
		out := renderFormulaChecklist(name, f.checklist(), 1)
		if len(f.checklist()) == 0 || len(out) > 6000 {
			t.Errorf("%s: checklist has %d steps and %d chars; want steps, bounded", name, len(f.checklist()), len(out))
		}
	}

	f, err := cooker.cookForRender("mol-polecat-work", town, "", vars)
	if err != nil {
		t.Fatalf("mol-polecat-work: %v", err)
	}
	if out := renderFormulaChecklist("mol-polecat-work", f.checklist(), 1); !strings.Contains(out, "TOWN OVERLAY LINE") || !strings.Contains(out, "gt-cook") {
		t.Errorf("step 1 lacks the overlay or the issue var:\n%s", out)
	}

	idea, err := cooker.cookForRender("mol-idea-to-plan", town, "", []string{"problem=Ship it"})
	if err != nil {
		t.Fatalf("mol-idea-to-plan: %v", err)
	}
	targets := map[string]string{}
	for _, s := range idea.Steps {
		targets[s.ID] = workflowStepTarget(s, "gastown")
		if s.ID == "human-clarify" && !s.metaBool("interactive") {
			t.Errorf("human-clarify lost metadata.interactive: %v", s.Metadata)
		}
		if s.ID == "prd-review" && !strings.Contains(s.Description, "Ship it") {
			t.Errorf("prd-review lacks the --set problem value")
		}
	}
	if targets["prd-review"] != "mayor" || targets["intake"] != "gastown" {
		t.Errorf("sling targets = %v; want prd-review on mayor, intake on the rig", targets)
	}

	got, err := realFormulaBD().varsForBead("mol-polecat-work", "gt-cook", "t", town, town, nil)
	if err != nil {
		t.Fatalf("varsForBead: %v", err)
	}
	want := []string{"feature=t", "issue=gt-cook", "base_branch=main", "build_command=", "lint_command=", "setup_command=", "test_command=", "typecheck_command="}
	if !slices.Equal(got, want) {
		t.Errorf("bond vars = %v, want %v", got, want)
	}
}

// checkCookedConvoy: a cooked convoy reads as legs with a focus plus a
// synthesis that needs every leg, the review convoys' legs are analysis-only,
// and its output paths and synthesis render with no placeholder left.
func checkCookedConvoy(t *testing.T, name string, f *cookedFormula) {
	t.Helper()
	p := convoyPlanFrom(f)
	if f.Type != "convoy" || len(p.Legs) == 0 || p.Synthesis == nil || p.BasePrompt == "" {
		t.Errorf("%s: type %s, %d legs, synthesis %v, base prompt %d chars", name, f.Type, len(p.Legs), p.Synthesis != nil, len(p.BasePrompt))
		return
	}
	if len(p.Synthesis.Needs) != len(p.Legs) {
		t.Errorf("%s: synthesis needs %v, want every leg", name, p.Synthesis.Needs)
	}
	wantReviewOnly := name == "mol-plan-review" || name == "mol-prd-review" // gt-kvf
	for _, leg := range p.Legs {
		if leg.Focus == "" || leg.ReviewOnly != wantReviewOnly {
			t.Errorf("%s: leg %s focus %q review_only %v", name, leg.ID, leg.Focus, leg.ReviewOnly)
		}
	}
	ctx := formulaTemplateContext(name, "local files", "abc123", 0, "", nil, nil,
		map[string]interface{}{"context": "extra context", "plan": "test plan", "prd_review": "prd-review.md", "problem": "test problem", "scope": "test scope"})
	dir, err := renderTemplate(p.OutputDir, ctx)
	if err != nil || !strings.HasSuffix(dir, "/abc123") {
		t.Errorf("%s: output directory = %q, %v", name, dir, err)
	}
	addOutputTemplateContext(ctx, dir, p.SynthesisFile)
	ctx["leg"] = map[string]interface{}{"id": p.Legs[0].ID, "title": p.Legs[0].Title, "focus": p.Legs[0].Focus, "description": p.Legs[0].Description}
	ctx["output_path"] = dir + "/" + renderTemplateOrDefault(p.LegPattern, ctx, "")
	for what, text := range map[string]string{"synthesis": p.Synthesis.Description, "base prompt": p.BasePrompt} {
		got, err := renderTemplate(text, ctx)
		if err != nil || strings.Contains(got, "{{") || strings.Contains(got, "<no value>") {
			t.Errorf("%s: %s left placeholders (%v): %q", name, what, err, got)
		}
	}
	if syn, _ := renderTemplate(p.Synthesis.Description, ctx); !strings.Contains(syn, dir) {
		t.Errorf("%s: synthesis does not name the output directory %s", name, dir)
	}
}
