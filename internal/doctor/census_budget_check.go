package doctor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/daemonlog"
)

const (
	// censusBudgetWindows is how many of the log's newest census records the
	// check measures: an hour at one record a minute, the baseline's window
	// (gt-gojo0).
	censusBudgetWindows = 60
	// censusBudgetMinWindows is the fewest records that say anything about a
	// caller's average. Below it the check reports not enough data rather than
	// passing on a log it has barely seen.
	censusBudgetMinWindows = 20
	// censusBudgetStale is how old the newest record may be before the check
	// reads the log as stopped: the daemon logs a record a minute, so five
	// missed ticks mean the census is no longer measuring (gt-gojo0).
	censusBudgetStale = 5 * time.Minute
	// censusBudgetTailBytes bounds the read: an hour of census records is a
	// few tens of kilobytes, so a megabyte covers the window with the daemon's
	// other lines interleaved.
	censusBudgetTailBytes = 1 << 20
)

// CensusBudgetCheck fails when a bd caller's average rate in the daemon's
// census exceeds the budget internal/beads/census_budgets.go records for it,
// and warns about a caller the table does not name (gt-gojo0). A window lists
// only its eight busiest rows, so an average is a floor: calls the log leaves
// out are never counted against a caller.
type CensusBudgetCheck struct {
	BaseCheck
	// Now is the clock the staleness guard reads; nil is time.Now.
	Now func() time.Time
	// Budgets replaces the shipped table; nil reads it from internal/beads.
	// Tests narrow it to drive the unbudgeted branch without editing the data.
	Budgets []beads.CensusBudget
}

// NewCensusBudgetCheck creates a new bd call budget check.
func NewCensusBudgetCheck() *CensusBudgetCheck {
	return &CensusBudgetCheck{
		BaseCheck: BaseCheck{
			CheckName:        "census-budget",
			CheckDescription: "Check every bd caller stays within its calls-per-minute budget",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run measures the newest census records in <town>/daemon/daemon.log.
func (c *CensusBudgetCheck) Run(ctx *CheckContext) *CheckResult {
	samples, err := censusSamples(ctx.TownRoot, censusBudgetWindows)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &CheckResult{Name: c.Name(), Status: StatusSkipped, Message: "no daemon log to read: not enough data"}
		}
		return &CheckResult{Name: c.Name(), Status: StatusSkipped, Message: fmt.Sprintf("cannot read daemon.log: %v: not enough data", err)}
	}
	if len(samples) < censusBudgetMinWindows {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: fmt.Sprintf("not enough data: %d census windows in the log, need %d", len(samples), censusBudgetMinWindows),
		}
	}
	if age := c.now().Sub(samples[len(samples)-1].At); age > censusBudgetStale {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("census stale: %s", age.Round(time.Second)),
			Details: []string{fmt.Sprintf("the newest census record in daemon.log is %s old, so the rates below it are not the town's", age.Round(time.Second))},
			FixHint: "Check that the daemon's census log still runs (gt daemon status, then daemon.log for its startup line)",
		}
	}

	calls := censusCallerCalls(samples)
	minutes := censusSpanMinutes(samples)
	budgets := make(map[string]float64)
	for _, b := range c.budgetTable() {
		budgets[b.Caller] = b.CallsPerMinute
	}

	var over, unbudgeted []string
	for _, caller := range sortedKeys(calls) {
		average := float64(calls[caller]) / minutes
		budget, ok := budgets[caller]
		switch {
		case !ok:
			unbudgeted = append(unbudgeted,
				fmt.Sprintf("%s averages %.1f calls/min over %d windows and has no budget", caller, average, len(samples)))
		case average > budget:
			over = append(over,
				fmt.Sprintf("%s averages %.1f calls/min over %d windows, budget %.1f", caller, average, len(samples), budget))
		}
	}

	switch {
	case len(over) > 0:
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d bd caller(s) over budget in the last %d census windows", len(over), len(samples)),
			Details: append(append([]string(nil), over...), unbudgeted...),
			FixHint: "Cut the caller's bd calls, or raise its row in internal/beads/census_budgets.go (a data edit om reviews)",
		}
	case len(unbudgeted) > 0:
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d bd caller(s) have no budget", len(unbudgeted)),
			Details: unbudgeted,
			FixHint: "Add each caller's average as a row in internal/beads/census_budgets.go",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("every bd caller is within budget across the last %d census windows", len(samples)),
	}
}

func (c *CensusBudgetCheck) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *CensusBudgetCheck) budgetTable() []beads.CensusBudget {
	if c.Budgets != nil {
		return c.Budgets
	}
	return beads.CensusBudgets()
}

// censusSample is one census record with the instant its log line carries.
type censusSample struct {
	At     time.Time
	Window beads.CensusWindow
}

// censusSamples returns the newest max census records in the town's daemon
// log, oldest first, because a burst older than the window must leave the
// measurement.
func censusSamples(townRoot string, max int) ([]censusSample, error) {
	lines, err := readLogTail(filepath.Join(townRoot, "daemon", daemonlog.FileName), censusBudgetTailBytes)
	if err != nil {
		return nil, err
	}
	// The log's stamps are local time with no zone, so the parser reads them
	// in the zone the staleness guard compares against.
	parser := daemonlog.Parser{Loc: time.Local}
	var samples []censusSample
	for _, line := range lines {
		at, text := parser.Parse(line)
		window, ok := beads.ParseCensusLine(text)
		if !ok {
			continue
		}
		// A record with no stamp of its own cannot be placed on the span, and
		// dropping it can only shorten the span, which raises every rate: the
		// omission fails toward an error, never toward a pass.
		if at.IsZero() {
			continue
		}
		samples = append(samples, censusSample{At: at, Window: window})
		if len(samples) > max {
			samples = samples[1:]
		}
	}
	return samples, nil
}

// censusCallerCalls folds the windows into each caller's call count. A window
// lists only its busiest rows, so the count is the calls the log can attribute.
func censusCallerCalls(samples []censusSample) map[string]int {
	calls := make(map[string]int)
	for _, sample := range samples {
		for _, row := range sample.Window.Rows {
			calls[row.Caller] += row.Count
		}
	}
	return calls
}

// censusSpanMinutes is the time the records cover: first stamp to last. A
// minute with no calls logs nothing, so dividing by the records' own windows
// would charge a caller's calls to the few minutes that happened to log and
// overstate its rate (gt-gojo0). The span never falls below one minute per
// record, so records written inside the same minute still average over it.
func censusSpanMinutes(samples []censusSample) float64 {
	minutes := samples[len(samples)-1].At.Sub(samples[0].At).Minutes()
	if minutes < float64(len(samples)) {
		minutes = float64(len(samples))
	}
	return minutes
}

// readLogTail returns the lines of path's last maxBytes.
func readLogTail(path string, maxBytes int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return tailLines(f, maxBytes)
}

// tailLines returns the lines of the last maxBytes of f. The read starts one
// byte before the tail, so the byte it lands on says whether the cut fell at a
// line boundary; a line the read began inside is dropped, because the bytes
// before the tail are gone.
func tailLines(f io.ReadSeeker, maxBytes int64) ([]string, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	start := size - maxBytes
	if start < 0 {
		start = 0
	}
	readFrom := start
	if start > 0 {
		readFrom = start - 1
	}
	if _, err := f.Seek(readFrom, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, size-readFrom))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	if start > 0 {
		lines = lines[1:]
	}
	return lines, nil
}

// sortedKeys returns m's keys in order, so a check's details do not reshuffle
// between runs.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
