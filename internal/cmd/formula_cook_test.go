package cmd

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCookForRender_CooksInRigDirWithTownOverlay: prime cooks where pour does
// (the rig dir, GT_ROOT the town) with the town's one overlay dir, and passes
// the vars through.
func TestCookForRender_CooksInRigDirWithTownOverlay(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := &fakeCook{out: cookTree(docAuditTree)}
	if _, err := (formulaCooker{open: fake.open}).cookForRender("mol-doc-audit", town, "gastown", []string{"issue=gt-1"}); err != nil {
		t.Fatalf("cookForRender: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("bd calls = %d, want 1", len(fake.calls))
	}
	c := fake.calls[0]
	if c.argv != "cook mol-doc-audit --var issue=gt-1" {
		t.Errorf("argv = %q", c.argv)
	}
	if want := (formulaSite{dir: filepath.Join(town, "gastown"), townRoot: town}); c.site != want {
		t.Errorf("site = %+v, want %+v", c.site, want)
	}
	env := envSlice(formulaEnv(c.site.townRoot))
	if env["GT_ROOT"] != town {
		t.Errorf("GT_ROOT = %q, want %q", env["GT_ROOT"], town)
	}
	if want := filepath.Join(town, "formula-overlays"); env["BD_FORMULA_OVERLAY_DIR"] != want {
		t.Errorf("BD_FORMULA_OVERLAY_DIR = %q, want %q", env["BD_FORMULA_OVERLAY_DIR"], want)
	}
}

// TestCookForRender_FailureIsOneLine: a formula bd cannot cook fails closed
// with one line naming the formula and bd's message.
func TestCookForRender_FailureIsOneLine(t *testing.T) {
	t.Parallel()
	fake := &fakeCook{msg: "invalid formula x.toml: line 7: steps.acceptance: unknown key\nsecond line"}
	_, err := (formulaCooker{open: fake.open}).cookForRender("shiny", "", "", nil)
	if err == nil {
		t.Fatal("cook failure must be an error")
	}
	if got, want := err.Error(), "cook formula shiny: invalid formula x.toml: line 7: steps.acceptance: unknown key"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestCookedChecklist_ParentsBeforeChildren(t *testing.T) {
	t.Parallel()
	var ids []string
	for _, s := range cookedFixture(t, docAuditTree).checklist() {
		ids = append(ids, s.ID)
	}
	if want := []string{"load-context", "audit", "audit.check"}; !slices.Equal(ids, want) {
		t.Errorf("checklist = %v, want %v", ids, want)
	}
}

func TestBackfillFormulaDefaultVars(t *testing.T) {
	t.Parallel()
	const tree = `{"formula": "mol-x", "vars": [
	  {"name": "base_branch", "required": false, "default": "main", "value": "main", "provided": false},
	  {"name": "issue", "required": true, "default": null, "value": "gt-1", "provided": true},
	  {"name": "note", "required": false, "default": null, "value": null, "provided": false},
	  {"name": "unused", "required": true, "default": null, "value": null, "provided": false}
	], "unresolved_vars": ["note"], "steps": []}`
	got, err := backfillFormulaDefaultVars(cookedFixture(t, tree), []string{"issue=gt-1"})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if want := []string{"issue=gt-1", "base_branch=main", "note="}; !slices.Equal(got, want) {
		t.Errorf("vars = %v, want %v", got, want)
	}

	// A caller's value is never replaced by the default.
	got, err = backfillFormulaDefaultVars(cookedFixture(t, tree), []string{"issue=gt-1", "base_branch=integration/epic-7"})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if want := []string{"issue=gt-1", "base_branch=integration/epic-7", "note="}; !slices.Equal(got, want) {
		t.Errorf("vars = %v, want %v", got, want)
	}
}

func TestBackfillFormulaDefaultVars_RequiredPlaceholderWithoutValue(t *testing.T) {
	t.Parallel()
	const tree = `{"formula": "mol-x", "vars": [
	  {"name": "scope", "required": true, "default": null, "value": null, "provided": false}
	], "unresolved_vars": ["scope"], "steps": []}`
	_, err := backfillFormulaDefaultVars(cookedFixture(t, tree), nil)
	if err == nil || !strings.Contains(err.Error(), "formula mol-x: required variable(s) scope have no default") {
		t.Fatalf("err = %v, want the missing required var named", err)
	}
}
