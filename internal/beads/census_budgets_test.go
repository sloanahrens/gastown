package beads

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// baselineCaller is one row of the baseline copy in testdata: the rate the
// census measured, the caller, and the bead that cuts it.
type baselineCaller struct {
	Rate   float64
	Caller string
	CutBy  string
}

// readCensusBaseline reads testdata/census-baseline-20261030.txt, the copy of
// the census baseline the budget table is built from, so a test can check the
// shipped table against the data it claims to hold and not only its length.
func readCensusBaseline(t *testing.T) []baselineCaller {
	t.Helper()
	f, err := os.Open("testdata/census-baseline-20261030.txt")
	if err != nil {
		t.Fatalf("open the baseline copy: %v", err)
	}
	defer func() { _ = f.Close() }()

	var rows []baselineCaller
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("baseline line %q is not rate, caller, cutting bead", line)
		}
		rate, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			t.Fatalf("baseline line %q: rate: %v", line, err)
		}
		rows = append(rows, baselineCaller{Rate: rate, Caller: fields[1], CutBy: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the baseline copy: %v", err)
	}
	return rows
}

// budgetFor is the budget a baseline row allows: the rate itself, rounded up to
// the next hundredth, when a hot-path bead cuts the caller (the budget is
// tightened after a post-install census), and the rate times 1.25 otherwise.
func budgetFor(row baselineCaller) float64 {
	if row.CutBy != "-" {
		return math.Ceil(row.Rate*100) / 100
	}
	return math.Ceil(row.Rate*1.25*100) / 100
}

// TestCensusBudgetsAreTheBaseline: the shipped table names the baseline's
// callers, in its order, at each one's baseline rate when a hot-path bead cuts it and at 1.25 times it when none does (gt-gojo0). A table
// that only counted sixteen rows would pass while naming the wrong sixteen or
// holding a budget the baseline never allowed.
func TestCensusBudgetsAreTheBaseline(t *testing.T) {
	baseline := readCensusBaseline(t)
	if len(baseline) != 16 {
		t.Fatalf("the baseline copy holds %d callers, want the top sixteen", len(baseline))
	}
	budgets := CensusBudgets()
	if len(budgets) != len(baseline) {
		t.Fatalf("the table names %d callers, the baseline %d", len(budgets), len(baseline))
	}
	for i, want := range baseline {
		got := budgets[i]
		if got.Caller != want.Caller {
			t.Errorf("row %d names %q, the baseline ranks %q there", i+1, got.Caller, want.Caller)
		}
		if wantBudget := budgetFor(want); got.CallsPerMinute != wantBudget {
			t.Errorf("%s: budget %v, want %v (baseline %v a minute, cut by %q)", got.Caller, got.CallsPerMinute, wantBudget, want.Rate, want.CutBy)
		}
	}
}

// TestCensusBudgetsNeverFallBelowTheBaseline: a budget under its own baseline
// rate fails the gate on the day it lands, so no row may be tightened until a
// census measures the cut.
func TestCensusBudgetsNeverFallBelowTheBaseline(t *testing.T) {
	baseline := readCensusBaseline(t)
	budgets := CensusBudgets()
	for i := range baseline {
		if budgets[i].CallsPerMinute < baseline[i].Rate {
			t.Errorf("%s: budget %v is below the baseline's %v a minute", budgets[i].Caller, budgets[i].CallsPerMinute, baseline[i].Rate)
		}
	}
}

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

// TestCensusBudgetCallerNamesMatchTheCensus: a row whose caller name differs
// from the name the census records never matches a row in the log, so the
// gate would report the caller unbudgeted every time it ran. The census names
// a caller "package.Func" from the frame that started the call, so a value
// receiver carries no "(*T)" and a method on a pointer receiver does.
func TestCensusBudgetCallerNamesMatchTheCensus(t *testing.T) {
	for _, b := range CensusBudgets() {
		pkg, fn, ok := strings.Cut(b.Caller, ".")
		if !ok || pkg == "" || fn == "" {
			t.Errorf("caller %q is not package.Func", b.Caller)
		}
		if strings.Contains(fn, ".func") {
			t.Errorf("caller %q holds a closure suffix the census strips", b.Caller)
		}
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
