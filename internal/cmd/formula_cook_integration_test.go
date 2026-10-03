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

// TestIntegrationFormulaCook renders every shipped formula through the real bd
// cook the way prime does (gt-fd2cu.1): each one cooks with no warning, a
// workflow renders a bounded checklist, the town overlay applies, and a
// workflow step keeps its sling target (gt-fd2cu.1.1).
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
