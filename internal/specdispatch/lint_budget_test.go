package specdispatch

import (
	"slices"
	"strings"
	"testing"
)

// sizeWithBudget is a Size body of first, the change budget line and a split
// clause, so a budget test varies only the budget.
func sizeWithBudget(first, budgetLine string) string {
	return first + "\n\n" + budgetLine + "\n\nSplits into its own MR."
}

// workerSizeWithBudget is sizeWithBudget for the usual one-worker,
// one-landing bead.
func workerSizeWithBudget(budgetLine string) string {
	return sizeWithBudget("One worker, one landing.", budgetLine)
}

// A bead that declares more added lines than the operator's maximum is refused
// with both numbers and the two ways to split it, so the author learns the rule
// and what to do from the refusal alone (gt-j0xfx5).
func TestRunawayBeadBudgetRefusesOverMaximum(t *testing.T) {
	t.Parallel()

	s := sizedSpec(workerSizeWithBudget("Change: at most 450 added lines in 3 files."))
	v := LintWith(s, defaultTemplate(), LintOptions{BudgetMaxLines: 300})
	if v.Route != RouteRefuse {
		t.Fatalf("bead over the budget got route %q: %s", v.Route, v.Line(s.ID))
	}
	if v.Field != "size" {
		t.Errorf("Field = %q, want size", v.Field)
	}
	for _, want := range []string{
		"450", "300",
		"split the bead by package", "one bead per package",
		"by test group", "one bead per group of acceptance boxes that share a fake",
	} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("Reason %q does not name %q", v.Reason, want)
		}
	}
}

// changeBudgetCase is one row of the budget gate's table: the Size body, the
// bead's labels, the settings, and whether the bead is refused.
type changeBudgetCase struct {
	name   string
	size   string
	labels []string
	opts   LintOptions
	refuse bool
	// wantReason lists the phrases a refusal must carry.
	wantReason []string
}

// The budget gate reads the line only when a setting turns it on: a declared
// budget over the maximum is refused, one at or under it passes, a missing line
// is refused only when the budget is required, and a line the grammar cannot
// read is refused either way. A bead for the overseer or the operator is not
// held to it at all (gt-j0xfx5).
func TestRunawayBeadBudgetGate(t *testing.T) {
	t.Parallel()

	const over = 300
	malformed := workerSizeWithBudget("Change: at most 450 lines.")
	atMax := workerSizeWithBudget("Change: at most 300 added lines in 3 files.")
	underMax := workerSizeWithBudget("Change: at most 250 added lines in 2 files.")
	noLine := "One worker, one landing."

	cases := []changeBudgetCase{
		{
			name: "the words in capitals",
			size: workerSizeWithBudget("CHANGE: AT MOST 450 ADDED LINES IN 3 FILES."),
			opts: LintOptions{BudgetMaxLines: over}, refuse: true,
			wantReason: []string{"450", "300"},
		},
		{
			name: "at the maximum",
			size: atMax, opts: LintOptions{BudgetMaxLines: over},
		},
		{
			name: "under the maximum",
			size: underMax, opts: LintOptions{BudgetMaxLines: over},
		},
		{
			name: "no line, not required",
			size: noLine, opts: LintOptions{BudgetMaxLines: over},
		},
		{
			name: "no line, required",
			size: noLine, opts: LintOptions{BudgetRequired: true}, refuse: true,
			wantReason: []string{"no change budget", changeBudgetGrammar},
		},
		{
			name: "malformed line, not required",
			size: malformed, opts: LintOptions{BudgetMaxLines: over}, refuse: true,
			wantReason: []string{changeBudgetGrammar, `"Change: at most 450 lines."`},
		},
		{
			name: "malformed line, required",
			size: malformed, opts: LintOptions{BudgetRequired: true}, refuse: true,
			wantReason: []string{changeBudgetGrammar},
		},
		{
			name:   "an overseer-only bead over the maximum",
			size:   sizeWithBudget("One overseer session of about an hour.", "Change: at most 450 added lines in 3 files."),
			labels: []string{"overseer-only"},
			opts:   LintOptions{BudgetMaxLines: over, BudgetRequired: true},
		},
		{
			name:   "an operator-only bead with a malformed line",
			size:   sizeWithBudget("One operator session of about an hour.", "Change: at most 450 lines."),
			labels: []string{"operator-only"},
			opts:   LintOptions{BudgetMaxLines: over},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := sizedSpec(tc.size, tc.labels...)
			v := LintWith(s, defaultTemplate(), tc.opts)
			if !tc.refuse {
				if !v.Clean() {
					t.Fatalf("bead refused: %s", v.Line(s.ID))
				}
				return
			}
			if v.Route != RouteRefuse {
				t.Fatalf("bead got route %q, want %q: %s", v.Route, RouteRefuse, v.Line(s.ID))
			}
			if v.Field != "size" {
				t.Errorf("Field = %q, want size", v.Field)
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(v.Reason, want) {
					t.Errorf("Reason %q does not name %q", v.Reason, want)
				}
			}
		})
	}
}

// With both settings at their off value the budget gate reads nothing: a bead
// with no line, one with a malformed line and one over any maximum all lint
// exactly as a bead without a budget does, verdict line and warnings alike
// (gt-j0xfx5).
func TestRunawayBeadBudgetOffChangesNothing(t *testing.T) {
	t.Parallel()

	plain := Lint(goodSpec(), defaultTemplate())
	if !plain.Clean() {
		t.Fatalf("plain bead refused: %s", plain.Line("gt-good"))
	}
	cases := []struct {
		name string
		size string
	}{
		{"no line", "One worker, one landing."},
		{"a malformed line", workerSizeWithBudget("Change: at most 450 lines.")},
		{"a line over every maximum", workerSizeWithBudget("Change: at most 450 added lines in 3 files.")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := sizedSpec(tc.size)
			v := LintWith(s, defaultTemplate(), LintOptions{})
			if !v.Clean() {
				t.Fatalf("bead refused with the gate off: %s", v.Line(s.ID))
			}
			if got, want := v.Line(s.ID), plain.Line("gt-good"); got != want {
				t.Errorf("Line = %q, want %q", got, want)
			}
			if len(v.Warnings) != 0 {
				t.Errorf("warnings %q with the gate off, want none", v.Warnings)
			}
			if shape := LintShape(s, defaultTemplate()); !shape.Clean() {
				t.Errorf("LintShape refused with the gate off: %s", shape.Line(s.ID))
			}
		})
	}
}

// A bead whose shape already fails still lists the budget refusal, so one gt
// spec lint --json prints everything the filer has to fix at once (gt-j0xfx5).
func TestRunawayBeadBudgetJoinsShapeRefusals(t *testing.T) {
	t.Parallel()

	s := sizedSpec(workerSizeWithBudget("Change: at most 450 added lines in 3 files."))
	s.Description = strings.Replace(s.Description, "## Gate\nmake gate\n", "", 1)
	v := LintWith(s, defaultTemplate(), LintOptions{BudgetMaxLines: 300})
	if v.Route != RouteRefuse {
		t.Fatalf("verdict route %q, want %q: %s", v.Route, RouteRefuse, v.Line(s.ID))
	}
	fields := make([]string, 0, len(v.Refusals))
	for _, r := range v.Refusals {
		fields = append(fields, r.Field)
	}
	for _, want := range []string{"## Gate", "size"} {
		if !slices.Contains(fields, want) {
			t.Errorf("refusals %v do not name %q", fields, want)
		}
	}
}
