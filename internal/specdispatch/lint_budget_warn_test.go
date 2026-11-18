package specdispatch

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// budgetWarnSpec is a work bead whose Size section declares line's change
// budget and whose acceptance list carries boxes items, so one test drives the
// budget warning without disturbing the rest of the shape. An empty line is a
// Size section with no budget at all.
func budgetWarnSpec(line string, boxes int) Spec {
	size := "One worker, one landing."
	if line != "" {
		size = workerSizeWithBudget(line)
	}
	s := sizedSpec(size)
	items := make([]string, boxes)
	for i := range items {
		items[i] = fmt.Sprintf("- [ ] box %d", i+1)
	}
	s.Acceptance = strings.Join(items, "\n")
	return s
}

// budgetWarn40x4 is the WARN line a bead declaring 40 lines over 4 boxes gets:
// the text changeBudgetWarnText formats for those numbers. Both tests that pin
// it read this one literal (gt-ed0hjd).
const budgetWarn40x4 = "declared 40 lines for 4 boxes: beads that landed on their first attempt added a median 34 lines per box"

// budgetWarnings keeps the change budget's WARN line from warnings, whatever its
// numbers, so a silence test fails on that line and not on any other advisory
// warning a bead carries.
func budgetWarnings(warnings []string) []string {
	return slices.DeleteFunc(slices.Clone(warnings), func(w string) bool {
		return !strings.HasPrefix(w, "declared ")
	})
}

// A bead that declares 40 added lines for four acceptance boxes declares under
// twenty lines per box, so the lint warns with the declared lines, the box
// count and the measured median. It is a warning: the bead still dispatches
// (gt-ed0hjd).
func TestRunawayBeadBudgetWarningNamesBothNumbers(t *testing.T) {
	t.Parallel()

	s := budgetWarnSpec("Change: at most 40 added lines in 2 files.", 4)
	v := LintWith(s, defaultTemplate(), LintOptions{})
	if !v.Clean() {
		t.Fatalf("bead refused: %s", v.Line(s.ID))
	}
	if !slices.Contains(v.Warnings, budgetWarn40x4) {
		t.Fatalf("Warnings = %q, want %q", v.Warnings, budgetWarn40x4)
	}
}

// A budget at or over twenty lines per box is not warned, and neither is a bead
// that declares no budget, one whose Change line does not parse, one with no
// boxes to share the budget between, or a non-work bead (gt-ed0hjd).
func TestRunawayBeadBudgetWarningStaysSilent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		line  string
		boxes int
		epic  bool
	}{
		{name: "120 lines over 4 boxes", line: "Change: at most 120 added lines in 3 files.", boxes: 4},
		{name: "no budget", boxes: 4},
		{name: "exactly 20 lines per box", line: "Change: at most 80 added lines in 2 files.", boxes: 4},
		{name: "40 lines with no boxes", line: "Change: at most 40 added lines in 2 files.", boxes: 0},
		{name: "a malformed Change line", line: "Change: at most 40 lines.", boxes: 4},
		{name: "an epic with a small budget", line: "Change: at most 40 added lines in 2 files.", boxes: 4, epic: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := budgetWarnSpec(tc.line, tc.boxes)
			if tc.epic {
				s.Type = "epic"
			}
			v := LintWith(s, defaultTemplate(), LintOptions{})
			if got := budgetWarnings(v.Warnings); len(got) != 0 {
				t.Errorf("budget warning = %q, want none", got)
			}
		})
	}
}

// The budget warning is advisory: it changes neither the route nor the exit
// code. A bead under twenty lines per box dispatches exactly as the same bead
// with no budget line does, and a bead refused for another reason keeps its
// refusal while carrying the warning (gt-ed0hjd).
func TestRunawayBeadBudgetWarningLeavesTheRoute(t *testing.T) {
	t.Parallel()

	budgeted := budgetWarnSpec("Change: at most 40 added lines in 2 files.", 4)
	plain := budgetWarnSpec("", 4)
	bv := LintWith(budgeted, defaultTemplate(), LintOptions{})
	pv := LintWith(plain, defaultTemplate(), LintOptions{})
	if bv.Route != pv.Route {
		t.Fatalf("route with a budget = %q, want the route without one, %q", bv.Route, pv.Route)
	}
	if bv.Route != RouteDispatch {
		t.Fatalf("warned bead got route %q, want %q", bv.Route, RouteDispatch)
	}

	refused := budgeted
	refused.Description = strings.Replace(refused.Description, "## Gate\nmake gate\n", "", 1)
	rv := LintWith(refused, defaultTemplate(), LintOptions{})
	if rv.Route != RouteRefuse {
		t.Fatalf("bead missing its Gate got route %q, want %q", rv.Route, RouteRefuse)
	}
	if !slices.Contains(rv.Warnings, budgetWarn40x4) {
		t.Errorf("Warnings = %q, want the budget warning beside the refusal", rv.Warnings)
	}
}
