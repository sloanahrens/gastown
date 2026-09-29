package testpolicy

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// overbudget.txt lists packages that meet every unit-test rule (so they are
// not in unconverted.txt) but still run longer than the converted-package
// time budget. Each line is "<package> <bead-id>", optionally followed by
// "# note"; the bead tracks getting the package under budget. The budget
// runner exempts these packages and reports their times on every run, and
// the list only shrinks (maxOverBudget in policy_test.go).

// OverBudgetEntry is one line of overbudget.txt. Bead is empty when the line
// names no bead.
type OverBudgetEntry struct {
	Package, Bead string
	Line          int
}

// TrackedRun is the time an over-budget package took in one budget run.
type TrackedRun struct {
	Package, Bead string
	Elapsed       time.Duration
}

var beadIDPattern = regexp.MustCompile(`^[a-z]+-[a-z0-9]+(\.[0-9]+)*$`)

// ParseOverBudget reads overbudget.txt's entries. It checks only the syntax
// needed to split a line; CheckOverBudget judges the entries.
func ParseOverBudget(r io.Reader) ([]OverBudgetEntry, error) {
	var entries []OverBudgetEntry
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		switch len(fields) {
		case 0:
			continue
		case 1:
			entries = append(entries, OverBudgetEntry{Package: fields[0], Line: n})
		case 2:
			entries = append(entries, OverBudgetEntry{Package: fields[0], Bead: fields[1], Line: n})
		default:
			return nil, fmt.Errorf("line %d: want \"<package> <bead-id> [# note]\", got %q", n, sc.Text())
		}
	}
	return entries, sc.Err()
}

// ReadOverBudget reads the overbudget.txt at path.
func ReadOverBudget(path string) ([]OverBudgetEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseOverBudget(f)
}

// CheckOverBudget returns one message per problem with entries. violations
// maps every Go package directory to its rule-violation count; unconverted is
// unconverted.txt; max is the ratchet (maxOverBudget).
func CheckOverBudget(entries []OverBudgetEntry, unconverted map[string]bool, violations map[string]int, max int) []string {
	var errs []string
	if len(entries) > max {
		errs = append(errs, fmt.Sprintf("overbudget.txt has %d entries, want at most %d: the list only shrinks — getting a package under budget deletes its line AND lowers maxOverBudget in policy_test.go", len(entries), max))
	}
	seen := map[string]int{}
	for _, e := range entries {
		where := fmt.Sprintf("overbudget.txt:%d: %s", e.Line, e.Package)
		if first, dup := seen[e.Package]; dup {
			errs = append(errs, fmt.Sprintf("%s is listed twice (first on line %d)", where, first))
			continue
		}
		seen[e.Package] = e.Line
		if !beadIDPattern.MatchString(e.Bead) {
			errs = append(errs, fmt.Sprintf("%s needs a bead id tracking the overrun (matching %s), got %q", where, beadIDPattern, e.Bead))
		}
		n, isPkg := violations[e.Package]
		switch {
		case !isPkg:
			errs = append(errs, fmt.Sprintf("%s is not a Go package directory", where))
		case unconverted[e.Package]:
			errs = append(errs, fmt.Sprintf("%s is also in unconverted.txt: a package is either unconverted or converted-but-over-budget", where))
		case n > 0:
			errs = append(errs, fmt.Sprintf("%s fails %d rule check(s): only a package that meets every rule may be listed as over budget", where, n))
		}
	}
	return errs
}
