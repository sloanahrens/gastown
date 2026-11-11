package beads

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// censusCopyRow is one row of a pinned census copy in testdata: the rate the
// census measured, the caller, and the bead that cuts it.
type censusCopyRow struct {
	Rate   float64
	Caller string
	CutBy  string
}

// readCensusRows reads one of the pinned census copies in testdata, so a test
// can check the shipped table against the data it claims to hold and not only
// its length.
func readCensusRows(t *testing.T, path string) []censusCopyRow {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	var rows []censusCopyRow
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("%s: line %q is not rate, caller, cutting bead", path, line)
		}
		rate, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			t.Fatalf("%s: line %q: rate: %v", path, line, err)
		}
		rows = append(rows, censusCopyRow{Rate: rate, Caller: fields[1], CutBy: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return rows
}

// readCensusBaseline reads testdata/census-baseline-20261030.txt, the copy of
// the census baseline the budget table is built from.
func readCensusBaseline(t *testing.T) []censusCopyRow {
	t.Helper()
	return readCensusRows(t, "testdata/census-baseline-20261030.txt")
}

// readCensusRebase reads testdata/census-rebase-20261101.txt, the explicit
// overrides a later census measured above the baseline.
func readCensusRebase(t *testing.T) []censusCopyRow {
	t.Helper()
	return readCensusRows(t, "testdata/census-rebase-20261101.txt")
}

// budgetFor is the budget a baseline row allows: the rate itself, rounded up to
// the next hundredth, when a hot-path bead cuts the caller (the budget is
// tightened after a post-install census), and the rate times 1.25 otherwise.
func budgetFor(row censusCopyRow) float64 {
	if row.CutBy != "-" {
		return math.Ceil(row.Rate*100) / 100
	}
	return math.Ceil(row.Rate*1.25*100) / 100
}

// TestCensusBudgetsAreTheBaseline: the shipped table names the baseline's
// callers, in its order, at the budget the censusCallerBudgets comment derives
// from each one (gt-gojo0), except a re-based row, which carries the rate
// testdata/census-rebase-20261101.txt records (gt-1rub5). A table that only
// counted sixteen rows would pass while naming the wrong sixteen or holding a
// budget the baseline never allowed.
func TestCensusBudgetsAreTheBaseline(t *testing.T) {
	t.Parallel()
	baseline := readCensusBaseline(t)
	rebased := readCensusRebase(t)
	if len(baseline) != 16 {
		t.Fatalf("the baseline copy holds %d callers, want the top sixteen", len(baseline))
	}
	budgets := CensusBudgets()
	if len(budgets) != len(baseline) {
		t.Fatalf("the table names %d callers, the baseline %d", len(budgets), len(baseline))
	}
	inBaseline := make(map[string]bool, len(baseline))
	for _, row := range baseline {
		inBaseline[row.Caller] = true
	}
	rebaseRate := make(map[string]float64, len(rebased))
	for _, row := range rebased {
		if !inBaseline[row.Caller] {
			t.Errorf("re-base row %q names no caller in the baseline", row.Caller)
		}
		if _, ok := rebaseRate[row.Caller]; ok {
			t.Errorf("re-base row %q appears twice", row.Caller)
		}
		rebaseRate[row.Caller] = row.Rate
	}
	for i, want := range baseline {
		got := budgets[i]
		if got.Caller != want.Caller {
			t.Errorf("row %d names %q, the baseline ranks %q there", i+1, got.Caller, want.Caller)
		}
		if rate, ok := rebaseRate[want.Caller]; ok {
			if got.CallsPerMinute != rate {
				t.Errorf("%s: budget %v, want the re-based %v (baseline %v a minute, cut by %q)", got.Caller, got.CallsPerMinute, rate, want.Rate, want.CutBy)
			}
			continue
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
	t.Parallel()
	baseline := readCensusBaseline(t)
	budgets := CensusBudgets()
	for i := range baseline {
		if budgets[i].CallsPerMinute < baseline[i].Rate {
			t.Errorf("%s: budget %v is below the baseline's %v a minute", budgets[i].Caller, budgets[i].CallsPerMinute, baseline[i].Rate)
		}
	}
}

// TestCensusBudgetScanRowIsTheMeasuredScan: the re-based scan row stands on the
// census windows recorded verbatim in testdata/census-recorded-20261101.txt, not
// on a number typed beside it: every recorded window shows the scan at one
// count, the override file carries that count, and the shipped budget equals it
// (gt-1rub5).
func TestCensusBudgetScanRowIsTheMeasuredScan(t *testing.T) {
	t.Parallel()
	const caller = "schedulerrun.listAllSlingContextRecords"
	counts := recordedCallerCounts(t, "testdata/census-recorded-20261101.txt", caller)
	if len(counts) == 0 {
		t.Fatalf("no recorded census window lists %s", caller)
	}
	measured := float64(counts[0])
	for _, n := range counts {
		if float64(n) != measured {
			t.Fatalf("the recorded windows show %s at %v, not one count", caller, counts)
		}
	}
	var override float64
	found := false
	for _, row := range readCensusRebase(t) {
		if row.Caller == caller {
			override, found = row.Rate, true
		}
	}
	if !found || override != measured {
		t.Errorf("the override for %s is %v (found %v), the recorded windows show %v", caller, override, found, measured)
	}
	for _, b := range CensusBudgets() {
		if b.Caller == caller && b.CallsPerMinute != measured {
			t.Errorf("%s: budget %v, want the recorded %v", caller, b.CallsPerMinute, measured)
		}
	}
}

// recordedCallerCounts returns, for each census window line in path that lists
// caller, the count it shows.
func recordedCallerCounts(t *testing.T, path, caller string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []int
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		_, rest, ok := strings.Cut(line, "@"+caller+"=")
		if !ok {
			continue
		}
		digits := rest
		if i := strings.IndexByte(rest, ' '); i >= 0 {
			digits = rest[:i]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("%s: window line shows %s with %q: %v", path, caller, digits, err)
		}
		out = append(out, n)
	}
	return out
}

// TestCensusRebaseRowsOnlyRise: an override is for a rate a later census
// measured ABOVE the baseline budget; one at or below it is an edit that cuts a
// budget past the shrink-only guard, so it fails here (gt-1rub5).
func TestCensusRebaseRowsOnlyRise(t *testing.T) {
	t.Parallel()
	baselineBudget := map[string]float64{}
	for _, row := range readCensusBaseline(t) {
		baselineBudget[row.Caller] = budgetFor(row)
	}
	for _, row := range readCensusRebase(t) {
		base, ok := baselineBudget[row.Caller]
		if !ok {
			t.Errorf("re-base row %q names no caller in the baseline", row.Caller)
			continue
		}
		if row.Rate <= base {
			t.Errorf("re-base row %s is %v, not above its baseline budget %v: an override may only raise", row.Caller, row.Rate, base)
		}
	}
}

// TestCensusBudgetsNameEachCallerOnce: a repeated caller would shadow its own
// row in the gate's lookup, so the data must not hold one.
func TestCensusBudgetsNameEachCallerOnce(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	budgets := CensusBudgets()
	budgets[0].CallsPerMinute = 1e9
	if CensusBudgets()[0].CallsPerMinute == 1e9 {
		t.Error("CensusBudgets hands out the shipped table itself")
	}
}

// TestCensusWrapperReadsCoverEveryWrapperRow: the census no longer names the
// retry wrapper, so a wrapper budget row with no read counting against it is
// dead data the gate can never apply (gt-krmal).
func TestCensusWrapperReadsCoverEveryWrapperRow(t *testing.T) {
	t.Parallel()
	budgeted := map[string]bool{}
	for _, b := range CensusBudgets() {
		budgeted[b.Caller] = true
	}
	reads := CensusWrapperReads()
	for _, b := range CensusBudgets() {
		if !strings.HasPrefix(b.Caller, "landworker.RetryBeads.") {
			continue
		}
		if len(reads[b.Caller]) == 0 {
			t.Errorf("%s is budgeted, but no census read counts against it", b.Caller)
		}
	}
	for row, rs := range reads {
		if !budgeted[row] {
			t.Errorf("censusWrapperReads names %q, which is no budget row", row)
		}
		if len(rs) == 0 {
			t.Errorf("%s lists no reads", row)
		}
		for _, r := range rs {
			if r.Verb == "" || r.Caller == "" {
				t.Errorf("%s holds an empty read %+v", row, r)
			}
		}
	}
}

// TestCensusWrapperReadsChargeEachReadOnce: a read counted under two rows has
// its calls charged to both budgets, and one of them is a number nobody
// measured.
func TestCensusWrapperReadsChargeEachReadOnce(t *testing.T) {
	t.Parallel()
	seen := map[CensusRead]string{}
	for row, rs := range CensusWrapperReads() {
		for _, r := range rs {
			if prev, ok := seen[r]; ok {
				t.Errorf("read %+v counts against %s and %s", r, prev, row)
			}
			seen[r] = row
		}
	}
}

// TestCensusWrapperReadsReturnACopy: the gate measures against the shipped
// reads, so a caller editing the returned slices must not change them.
func TestCensusWrapperReadsReturnACopy(t *testing.T) {
	t.Parallel()
	for row := range CensusWrapperReads() {
		reads := CensusWrapperReads()
		reads[row][0].Caller = "mutated"
		if CensusWrapperReads()[row][0].Caller == "mutated" {
			t.Fatalf("CensusWrapperReads hands out the shipped reads of %s", row)
		}
	}
}
