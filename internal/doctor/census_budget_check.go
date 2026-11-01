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
// and warns about a caller the table does not name (gt-gojo0). The census names
// the landing step that decided to read rather than the retry wrapper it used
// to name, so those reads are summed onto the wrapper's row (gt-krmal).
//
// A window lists only its eight busiest rows. For a caller the table names
// directly, an average is a floor: calls the log leaves out are never counted
// against it, and the baseline measured the same floor. A folded wrapper row is
// different: its measured rate was one key's count, and the sum of the step
// reads behind it spreads over many keys that mostly miss the window's eight
// rows. When a window's listed rows account for fewer calls than its total, the
// folded rows are reported but neither passed nor failed, and the check reports
// the window incomplete (gt-krmal).
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
//
// The four ways the log can say nothing — absent, unreadable, too short, and
// stopped — are four different messages, because only one of them is a hook
// that has never run. None of them passes.
func (c *CensusBudgetCheck) Run(ctx *CheckContext) *CheckResult {
	logPath := censusLogPath(ctx.TownRoot)
	read, err := censusSamples(ctx.TownRoot, censusBudgetWindows)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &CheckResult{
				Name:    c.Name(),
				Status:  StatusSkipped,
				Message: fmt.Sprintf("no daemon log at %s: not enough data", logPath),
				FixHint: "Start the daemon so it logs a census record a minute (gt daemon start)",
			}
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("cannot read the census in %s: %v", logPath, err),
			Details: []string{"an unreadable log is not a clean census: the callers it holds were never measured"},
			FixHint: "Check the log's permissions and that the daemon is writing it",
		}
	}
	samples := read.Samples
	if len(samples) < censusBudgetMinWindows {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: fmt.Sprintf("not enough data: %d census windows in %s, need %d", len(samples), logPath, censusBudgetMinWindows),
			Details: censusNoteDetails(read),
		}
	}
	if age := c.now().Sub(samples[len(samples)-1].At); age > censusBudgetStale {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("census stale: %s", age.Round(time.Second)),
			Details: append([]string{fmt.Sprintf("the newest census record in %s is %s old, so the rates below it are not the town's", logPath, age.Round(time.Second))},
				censusNoteDetails(read)...),
			FixHint: "Check that the daemon's census log still runs (gt daemon status, then daemon.log for its startup line)",
		}
	}

	minutes := censusSpanMinutes(samples)
	budgets := make(map[string]float64)
	for _, b := range c.budgetTable() {
		budgets[b.Caller] = b.CallsPerMinute
	}
	folded := censusWrapperRows(budgets)
	foldedRows := make(map[string]bool, len(folded))
	for _, row := range folded {
		foldedRows[row] = true
	}
	rowCalls, unbudgetedCalls := censusReadRows(censusReadCalls(samples), budgets, folded)
	listing, total := censusListedAndTotal(samples)
	complete := listing == total

	var over, unbudgeted, visible []string
	for _, row := range sortedKeys(rowCalls) {
		average := float64(rowCalls[row]) / minutes
		if !complete && foldedRows[row] {
			// The listed rows are a sample of the window, so their sum is a
			// floor below the rate the wrapper rows were measured at. A floor
			// over the budget still proves a breach, so it fails; a floor under
			// it proves nothing, so the row is reported and left ungated rather
			// than passed on an undercount.
			if average > budgets[row] {
				over = append(over,
					fmt.Sprintf("%s shows at least %.1f calls/min over %d windows (a floor: the window lists only its busiest rows), budget %.1f", row, average, len(samples), budgets[row]))
				continue
			}
			visible = append(visible,
				fmt.Sprintf("%s shows %.1f calls/min over %d windows, budget %.1f: not gated, the window lists only its busiest rows", row, average, len(samples), budgets[row]))
			continue
		}
		if average > budgets[row] {
			over = append(over,
				fmt.Sprintf("%s averages %.1f calls/min over %d windows, budget %.1f", row, average, len(samples), budgets[row]))
		}
	}
	for _, caller := range sortedKeys(unbudgetedCalls) {
		average := float64(unbudgetedCalls[caller]) / minutes
		unbudgeted = append(unbudgeted,
			fmt.Sprintf("%s averages %.1f calls/min over %d windows and has no budget", caller, average, len(samples)))
	}
	// incomplete names the calls the measured windows counted but did not list.
	incomplete := fmt.Sprintf("incomplete: %d of %d reads visible", listing, total)

	switch {
	case len(over) > 0:
		details := append([]string(nil), over...)
		if !complete {
			details = append(details, incomplete+": the retry-wrapper rows were not gated on a partial fold")
		}
		details = append(append(details, visible...), unbudgeted...)
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d bd caller(s) over budget in the last %d census windows", len(over), len(samples)),
			Details: append(details, censusNoteDetails(read)...),
			FixHint: "Cut the caller's bd calls, or raise its row in internal/beads/census_budgets.go (a data edit om reviews)",
		}
	case !complete:
		details := append(append(append([]string(nil), visible...), unbudgeted...), censusNoteDetails(read)...)
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%s across the last %d census windows: the retry-wrapper rows are not gated on a partial fold", incomplete, len(samples)),
			Details: details,
			FixHint: "A window lists only its busiest rows, so a sum of them misses calls: the wrapper rows are reported but not gated until a window's rows account for its total",
		}
	case len(unbudgeted) > 0:
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d bd caller(s) have no budget", len(unbudgeted)),
			Details: append(unbudgeted, censusNoteDetails(read)...),
			FixHint: "Add each caller's average as a row in internal/beads/census_budgets.go",
		}
	}
	if read.Torn > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("within budget, but %d torn census record(s) were ignored: the averages read low", read.Torn),
			Details: censusNoteDetails(read),
			FixHint: "Check the daemon log for a crash or a full disk around the torn lines",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("every bd caller is within budget across the last %d census windows", len(samples)),
		Details: censusNoteDetails(read),
	}
}

// censusLogPath is the file the daemon writes its census to.
func censusLogPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", daemonlog.FileName)
}

// censusIgnoredDetails names the census records the read could not place,
// because an average taken over a window that silently dropped records is an
// average over data the operator cannot see.
func censusIgnoredDetails(ignored int) []string {
	if ignored == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d census record(s) ignored: each sits above the log's first stamped line, so it has no instant to be averaged over", ignored)}
}

// censusNoteDetails names what the read left out: records with no instant and
// records the log tore. Both leave an average over data the operator cannot see.
func censusNoteDetails(read censusRead) []string {
	out := censusIgnoredDetails(read.Ignored)
	if read.Torn > 0 {
		out = append(out, fmt.Sprintf("%d torn census record(s) ignored: their minutes are in the span and their calls are not, so the averages read low", read.Torn))
	}
	return out
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

// censusRead is what a log tail yielded: the census records that carry an
// instant, and how many could not be placed.
type censusRead struct {
	Samples []censusSample
	// Ignored is the records the read left out. A census record is one line,
	// and the log stamps every line, so a record without an instant is one
	// written above the log's first stamped line; with no moment to sit at, its
	// calls cannot be charged to a minute of the span.
	Ignored int
	// Torn is the lines that carry the census marker and do not parse: a record
	// cut by a crash, a full disk or a concurrent writer. The span counts the
	// minutes such a record sat in but not its calls, so every average is read
	// low by what they held; the check says so and does not report a clean pass.
	Torn int
}

// censusSamples returns the newest max census records in the town's daemon
// log, oldest first, because a burst older than the window must leave the
// measurement.
func censusSamples(townRoot string, max int) (censusRead, error) {
	lines, err := readLogTail(censusLogPath(townRoot), censusBudgetTailBytes)
	if err != nil {
		return censusRead{}, err
	}
	// The log's stamps are local time with no zone, so the parser reads them
	// in the zone the staleness guard compares against.
	parser := daemonlog.Parser{Loc: time.Local}
	var read censusRead
	for _, line := range lines {
		at, text := parser.Parse(line)
		window, ok := beads.ParseCensusLine(text)
		if !ok {
			if beads.IsCensusLine(text) {
				read.Torn++
			}
			continue
		}
		if at.IsZero() {
			read.Ignored++
			continue
		}
		read.Samples = append(read.Samples, censusSample{At: at, Window: window})
		if len(read.Samples) > max {
			read.Samples = read.Samples[1:]
		}
	}
	return read, nil
}

// censusReadKey is one row key: the verb and caller the census names. A caller
// that reads under two verbs is two reads, which is what lets a folded sum
// charge each read to the right budget row.
type censusReadKey struct{ verb, caller string }

// censusReadCalls folds the windows into each read's call count. A window
// lists only its busiest rows, so the count is the calls the log can attribute.
func censusReadCalls(samples []censusSample) map[censusReadKey]int {
	calls := make(map[censusReadKey]int)
	for _, sample := range samples {
		for _, row := range sample.Window.Rows {
			calls[censusReadKey{verb: row.Verb, caller: row.Caller}] += row.Count
		}
	}
	return calls
}

// censusListedAndTotal is the calls the measured windows' listed rows account
// for and the calls their totals count. A window's total covers every call it
// saw, so a listed sum below it means the log left rows out.
func censusListedAndTotal(samples []censusSample) (listing, total int) {
	for _, sample := range samples {
		listing += beads.CensusRowsTotal(sample.Window.Rows)
		total += sample.Window.Total
	}
	return listing, total
}

// censusWrapperRows maps each read a retry-wrapper budget row counts to that
// row, dropping rows the gate's table does not hold.
func censusWrapperRows(budgets map[string]float64) map[censusReadKey]string {
	rows := make(map[censusReadKey]string)
	for row, reads := range beads.CensusWrapperReads() {
		if _, ok := budgets[row]; !ok {
			continue
		}
		for _, read := range reads {
			rows[censusReadKey{verb: read.Verb, caller: read.Caller}] = row
		}
	}
	return rows
}

// censusReadRows charges every read to the budget row it counts against: its
// own caller when the table budgets that name, else the retry-wrapper row whose
// call sites the census names instead (gt-krmal). A read no row names is folded
// by caller for the unbudgeted report.
func censusReadRows(calls map[censusReadKey]int, budgets map[string]float64, folded map[censusReadKey]string) (rowCalls, unbudgeted map[string]int) {
	rowCalls = make(map[string]int)
	unbudgeted = make(map[string]int)
	for read, count := range calls {
		if _, ok := budgets[read.caller]; ok {
			rowCalls[read.caller] += count
			continue
		}
		if row, ok := folded[read]; ok {
			rowCalls[row] += count
			continue
		}
		unbudgeted[read.caller] += count
	}
	return rowCalls, unbudgeted
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
