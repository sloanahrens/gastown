package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

// TestBackfillFormulaDefaultVarsRealFormulas pins the backfill for formulas that
// ship with gt. bd's bond requires a value for every {{placeholder}} in the
// cooked proto and ignores [vars] defaults, so gt has to pass them — for every
// formula, not just mol-polecat-work (gt-25wi).
//
// The invariant asserted for every case: after backfill, each variable the
// formula declares a default for is present, and no variable the formula uses as
// a placeholder is left without a value.
func TestBackfillFormulaDefaultVarsRealFormulas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		formula    string
		extraVars  []string
		wantVars   []string // entries that must be present
		wantExact  []string // when set, the full list, in order
		wantAbsent []string // entries that must NOT be added
	}{
		{
			// The one formula the old code special-cased: its behavior must not
			// change — same entries, and nothing extra. issue is required and
			// supplied by the caller; every other declared var is defaulted.
			name:    "mol-polecat-work is unchanged",
			formula: "mol-polecat-work",
			wantExact: []string{
				"feature=t", "issue=gt-abc",
				"base_branch=main", "build_command=", "lint_command=",
				"setup_command=", "test_command=", "typecheck_command=",
			},
		},
		{
			// The reported failure: slinging this formula with no --var flags
			// died on missing required variables.
			name:    "mol-doc-audit gets its slice and command defaults",
			formula: "mol-doc-audit",
			wantVars: []string{
				"feature=t", "issue=gt-abc",
				"base_branch=main", "slice_docs=8", "slice_go=4",
				"build_command=", "lint_command=", "setup_command=",
				"test_command=", "typecheck_command=",
			},
		},
		{
			// A declared default that is not the empty string.
			name:    "mol-digest-generate keeps its non-empty default",
			formula: "mol-digest-generate",
			wantVars: []string{
				"feature=t", "issue=gt-abc", "period=daily",
			},
		},
		{
			// Declares no vars of its own: the defaults come from the parent
			// through extends, so the backfill has to resolve the chain.
			name:    "mol-polecat-work-monorepo-tdd inherits its parent's vars",
			formula: "mol-polecat-work-monorepo-tdd",
			wantVars: []string{
				"feature=t", "issue=gt-abc",
				"base_branch=main", "build_command=", "lint_command=",
				"setup_command=", "test_command=", "typecheck_command=",
			},
		},
		{
			// A required var the formula never interpolates must not be treated
			// as missing: bd does not demand it, so failing here would block a
			// bond that works today. (target/reason/warrant_id are {braced} prose
			// substitution in this formula, not {{placeholders}}.)
			name:       "mol-shutdown-dance does not demand its un-interpolated required vars",
			formula:    "mol-shutdown-dance",
			wantAbsent: []string{"target=", "reason=", "warrant_id="},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := formulaVarsForBead(tc.formula, "gt-abc", "t", "", tc.extraVars)
			if err != nil {
				t.Fatalf("formulaVarsForBead(%s): %v", tc.formula, err)
			}

			if tc.wantExact != nil {
				if strings.Join(got, ",") != strings.Join(tc.wantExact, ",") {
					t.Errorf("--vars = %v, want %v", got, tc.wantExact)
				}
			}

			have := make(map[string]bool, len(got))
			for _, v := range got {
				have[v] = true
			}
			for _, want := range tc.wantVars {
				if !have[want] {
					t.Errorf("missing --var %q in %v", want, got)
				}
			}
			for _, unwanted := range tc.wantAbsent {
				if have[unwanted] {
					t.Errorf("unexpected --var %q in %v", unwanted, got)
				}
			}

			assertEveryDeclaredVarResolved(t, tc.formula, "", "", got)
		})
	}
}

// TestBackfillFormulaDefaultVarsCallerValuesWin pins that a caller-supplied value
// is never replaced by the formula default.
func TestBackfillFormulaDefaultVarsCallerValuesWin(t *testing.T) {
	t.Parallel()

	got, err := backfillFormulaDefaultVars("mol-polecat-work",
		[]string{"issue=gt-abc", "base_branch=integration/epic-7"}, "", "")
	if err != nil {
		t.Fatalf("backfillFormulaDefaultVars: %v", err)
	}

	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "base_branch=integration/epic-7") {
		t.Errorf("caller's base_branch was overwritten: %v", got)
	}
	if strings.Contains(joined, "base_branch=main") {
		t.Errorf("formula default replaced the caller's value: %v", got)
	}
}

// TestBackfillFormulaDefaultVarsFailsLoudOnRequiredVar covers a required variable
// with no default and no value: gt must refuse with a message naming the formula
// and the variable, rather than let bd fail with its own generic error.
func TestBackfillFormulaDefaultVarsFailsLoudOnRequiredVar(t *testing.T) {
	t.Parallel()

	// mol-orphan-scan declares `scope` as required with no default.
	_, err := backfillFormulaDefaultVars("mol-orphan-scan", []string{"issue=gt-abc"}, "", "")
	if err == nil {
		t.Fatal("expected an error for the unset required variable, got nil")
	}
	if !strings.Contains(err.Error(), "mol-orphan-scan") || !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should name the formula and the missing variable, got: %v", err)
	}

	// Supplying it clears the failure.
	if _, err := backfillFormulaDefaultVars("mol-orphan-scan",
		[]string{"issue=gt-abc", "scope=rig"}, "", ""); err != nil {
		t.Errorf("supplied required var should satisfy the backfill: %v", err)
	}
}

// TestBackfillFormulaDefaultVarsReadsRigLocalFormula covers a formula that exists
// only on disk, under the rig tier of formula resolution, with a non-empty
// default: the backfill must read that file, not a compiled-in copy.
func TestBackfillFormulaDefaultVarsReadsRigLocalFormula(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	rigName := "testrig"
	dir := filepath.Join(townRoot, rigName, ".beads", "formulas")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir formulas dir: %v", err)
	}
	local := `formula = "mol-local-check"
description = "Test formula."
[[steps]]
id = "one"
title = "Step with {{tag}} and {{mode}}"
description = "and {{scope}}"
[vars.tag]
description = "A non-empty default"
default = "v1"
[vars.mode]
description = "An empty default"
default = ""
[vars.scope]
description = "Required, no default"
required = true
[vars.unused_required]
description = "Required but never interpolated: bd does not demand it"
required = true
`
	if err := os.WriteFile(filepath.Join(dir, "mol-local-check.formula.toml"), []byte(local), 0o600); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	got, err := backfillFormulaDefaultVars("mol-local-check", []string{"scope=full"}, townRoot, rigName)
	if err != nil {
		t.Fatalf("backfillFormulaDefaultVars: %v", err)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "tag=v1") {
		t.Errorf("non-empty default not backfilled: %v", got)
	}
	if !strings.Contains(joined, "mode=") {
		t.Errorf("empty default not backfilled: %v", got)
	}
	if !strings.Contains(joined, "scope=full") {
		t.Errorf("caller's required value dropped: %v", got)
	}
	if strings.Contains(joined, "unused_required") {
		t.Errorf("un-interpolated required var should not be backfilled: %v", got)
	}
}

// TestBackfillFormulaDefaultVarsUnknownFormula pins the fallback: when gt cannot
// load the formula, it passes the caller's vars through untouched and lets bd —
// which resolves the formula itself — report the real problem.
func TestBackfillFormulaDefaultVarsUnknownFormula(t *testing.T) {
	t.Parallel()

	in := []string{"feature=t", "issue=gt-abc"}
	got, err := backfillFormulaDefaultVars("mol-does-not-exist", in, "", "")
	if err != nil {
		t.Fatalf("unknown formula must not fail the backfill: %v", err)
	}
	if strings.Join(got, ",") != strings.Join(in, ",") {
		t.Errorf("got %v, want %v", got, in)
	}
}

// assertEveryDeclaredVarResolved checks the bond contract for a formula: every
// variable it uses as a placeholder and declares in [vars] has a value in vars.
// bd's bond rejects the call otherwise ("missing required variables").
func assertEveryDeclaredVarResolved(t *testing.T, formulaName, townRoot, rigName string, vars []string) {
	t.Helper()

	content, err := formula.ResolveFormulaContent(formulaName, townRoot, rigName)
	if err != nil {
		t.Fatalf("resolve formula content %s: %v", formulaName, err)
	}
	f, err := formula.Parse(content)
	if err != nil {
		t.Fatalf("parse formula %s: %v", formulaName, err)
	}

	var text strings.Builder
	text.WriteString(f.Description)
	for _, step := range f.Steps {
		text.WriteString(step.Title + " " + step.Description)
	}

	supplied := make(map[string]bool, len(vars))
	for _, v := range vars {
		supplied[v[:strings.Index(v, "=")]] = true
	}
	for _, used := range formula.ExtractTemplateVariables(text.String()) {
		if _, declared := f.Vars[used]; !declared {
			continue // documentation handlebars, not a formula input
		}
		if !supplied[used] {
			t.Errorf("formula %s uses {{%s}} but no --var was passed for it", formulaName, used)
		}
	}
}
