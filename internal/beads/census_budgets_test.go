package beads

import "testing"

// censusBudgetCallers is how many callers the gate budgets: the baseline's top
// sixteen, so the table covers the census's own ranking and not a sample of it
// (gt-gojo0).
const censusBudgetCallers = 16

// TestCensusBudgetsNameEachCallerOnce: a repeated caller would shadow its own
// row in the gate's lookup, so the data must not hold one.
func TestCensusBudgetsNameEachCallerOnce(t *testing.T) {
	seen := map[string]bool{}
	for _, b := range CensusBudgets() {
		if b.Caller == "" {
			t.Errorf("budget %+v names no caller", b)
		}
		if b.CallsPerMinute <= 0 {
			t.Errorf("budget %+v has no positive rate", b)
		}
		if seen[b.Caller] {
			t.Errorf("caller %q is budgeted twice", b.Caller)
		}
		seen[b.Caller] = true
	}
}

// TestCensusBudgetsCoverTheTopSixteen: the baseline ranked sixteen callers, and
// a table shorter than that leaves the rest of the ranking unbudgeted.
func TestCensusBudgetsCoverTheTopSixteen(t *testing.T) {
	if got := len(CensusBudgets()); got != censusBudgetCallers {
		t.Errorf("the table names %d callers, want the baseline's top %d", got, censusBudgetCallers)
	}
}

// TestCensusBudgetsMarkTheCutCallers: a caller another hot-path bead is cutting
// carries the target flag, so a reader of the data can tell a held cut from a
// measured baseline.
func TestCensusBudgetsMarkTheCutCallers(t *testing.T) {
	targets := 0
	for _, b := range CensusBudgets() {
		if b.Target {
			targets++
		}
	}
	if targets == 0 {
		t.Error("no row is marked Target, so the table reads as all baseline")
	}
}

// TestCensusBudgetsReturnACopy: the gate measures against the shipped data, so
// a caller editing the returned slice must not change the table.
func TestCensusBudgetsReturnACopy(t *testing.T) {
	budgets := CensusBudgets()
	budgets[0].CallsPerMinute = 1e9
	if CensusBudgets()[0].CallsPerMinute == 1e9 {
		t.Error("CensusBudgets hands out the shipped table itself")
	}
}
