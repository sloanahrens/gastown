package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/cacheneed"
	"github.com/steveyegge/gastown/internal/jobledger"
	"github.com/steveyegge/gastown/internal/workspace"
)

// cacheNeedTailRuns is more runs than the ledger's 1 MiB live file holds.
const cacheNeedTailRuns = 10000

// cacheNeedJobs are the cache trims the table reports, in table order.
var cacheNeedJobs = []string{
	"sweep_cache_trim", "ci_cache_trim:ci-gocache", "ci_cache_trim:ci-gomod", "ci_cache_trim:ci-npm",
}

var cacheCmd = newCacheCmd(cacheNeedRealDeps())

func init() { rootCmd.AddCommand(cacheCmd) }

// cacheNeedDeps are the seams a test substitutes: the town, the clock, the read.
// The read also returns the ledger's skipped-line count, which counts every
// undecodable line in the ledger, not only this job's.
type cacheNeedDeps struct {
	findTown func() (string, error)
	now      func() time.Time
	tail     func(townRoot, job string, n int) ([]jobledger.Run, int, error)
}

func cacheNeedRealDeps() cacheNeedDeps {
	return cacheNeedDeps{
		findTown: workspace.FindFromCwdOrError,
		now:      time.Now,
		tail: func(townRoot, job string, n int) ([]jobledger.Run, int, error) {
			ledger, err := jobledger.NewLedger(townRoot)
			if err != nil {
				return nil, 0, err
			}
			runs, err := ledger.Tail(job, n)
			if err != nil {
				return nil, 0, err
			}
			return runs, ledger.Skipped(), nil
		},
	}
}

func newCacheCmd(deps cacheNeedDeps) *cobra.Command {
	c := &cobra.Command{Use: "cache", Short: "Report what each cache needs", GroupID: GroupDiag, RunE: requireSubcommand}
	c.AddCommand(newCacheNeedCmd(deps))
	return c
}

func newCacheNeedCmd(deps cacheNeedDeps) *cobra.Command {
	var days, headroom int
	var asJSON bool
	c := &cobra.Command{
		Use:   "need",
		Short: "Print each cache's measured need beside its cap",
		Long: `A job with no ok run in the window prints "no data". The window is the
counted runs' own, so a ledger rotated by size shows less than --days.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if days < 1 {
				return &ExitCodeError{Code: 2, Err: fmt.Errorf("--days must be at least 1, got %d", days)}
			}
			return runCacheNeed(cmd.OutOrStdout(), cmd.ErrOrStderr(), deps, days, headroom, asJSON)
		},
	}
	c.Flags().IntVar(&days, "days", 7, "Count runs ending within this many days")
	c.Flags().IntVar(&headroom, "headroom-percent", 25, "Add this percent of the largest young figure to the need")
	c.Flags().BoolVar(&asJSON, "json", false, "Print the rows as a JSON array")
	return c
}

// cacheNeedRow is one job's line or JSON object. A nil figure is one no counted
// run recorded, and NoData marks a job with no usable run at all.
type cacheNeedRow struct {
	Job        string     `json:"job"`
	NoData     bool       `json:"no_data,omitempty"`
	Runs       *int       `json:"runs,omitempty"`
	FirstEnd   *time.Time `json:"first_end,omitempty"`
	LastEnd    *time.Time `json:"last_end,omitempty"`
	YoungBytes *int64     `json:"young_bytes,omitempty"`
	SizeBytes  *int64     `json:"size_bytes,omitempty"`
	CapBytes   *int64     `json:"cap_bytes,omitempty"`
	NeedBytes  *int64     `json:"need_bytes,omitempty"`
}

// cacheNeedRows puts one row per job in table order, so a job with no row from
// Compute is printed as no data rather than omitted.
func cacheNeedRows(byJob map[string]cacheneed.Row) []cacheNeedRow {
	rows := make([]cacheNeedRow, 0, len(cacheNeedJobs))
	for _, job := range cacheNeedJobs {
		row, ok := byJob[job]
		if !ok {
			rows = append(rows, cacheNeedRow{Job: job, NoData: true})
			continue
		}
		runs, young, need := row.Runs, row.MaxYoungBytes, row.Need
		first, last := row.FirstEnd.UTC(), row.LastEnd.UTC()
		rows = append(rows, cacheNeedRow{Job: job, Runs: &runs, FirstEnd: &first, LastEnd: &last,
			YoungBytes: &young, SizeBytes: row.MaxClaimBytesBefore, CapBytes: row.CapBytes, NeedBytes: &need})
	}
	return rows
}

// runCacheNeed reads every job's runs and prints one row per job. A failed
// ledger read returns before anything is printed. Undecodable ledger lines
// warn once on errOut and leave the rows and the result unchanged.
func runCacheNeed(out, errOut io.Writer, deps cacheNeedDeps, days, headroom int, asJSON bool) error {
	townRoot, err := deps.findTown()
	if err != nil {
		return fmt.Errorf("find the town root: %w", err)
	}
	var runs []jobledger.Run
	skipped := 0
	for _, job := range cacheNeedJobs {
		got, n, err := deps.tail(townRoot, job, cacheNeedTailRuns)
		if err != nil {
			return fmt.Errorf("read the job ledger for %s: %w", job, err)
		}
		runs = append(runs, got...)
		skipped = max(skipped, n)
	}
	if skipped > 0 {
		if _, err := fmt.Fprintf(errOut, "gt cache need: %d ledger line(s) could not be decoded; the figures may be understated\n", skipped); err != nil {
			return err
		}
	}
	rows, err := cacheneed.Compute(runs, deps.now().Add(-time.Duration(days)*24*time.Hour), headroom)
	if err != nil {
		return err
	}
	byJob := make(map[string]cacheneed.Row, len(rows))
	for _, row := range rows {
		byJob[row.Job] = row
	}
	printed := cacheNeedRows(byJob)
	if asJSON {
		if err := json.NewEncoder(out).Encode(printed); err != nil {
			return fmt.Errorf("write the rows as JSON: %w", err)
		}
		return nil
	}
	for _, row := range printed {
		if _, err := io.WriteString(out, cacheNeedLine(row)); err != nil {
			return err
		}
	}
	return nil
}

func cacheNeedLine(row cacheNeedRow) string {
	if row.NoData {
		return row.Job + " no data\n"
	}
	window := row.FirstEnd.Format(time.RFC3339) + ".." + row.LastEnd.Format(time.RFC3339)
	return fmt.Sprintf("%s runs=%d window=%s young_bytes=%d size_bytes=%s cap_bytes=%s need_bytes=%d\n",
		row.Job, *row.Runs, window, *row.YoungBytes, cacheNeedFig(row.SizeBytes), cacheNeedFig(row.CapBytes), *row.NeedBytes)
}

// cacheNeedFig prints an absent figure as "none", never as zero.
func cacheNeedFig(b *int64) string {
	if b == nil {
		return "none"
	}
	return strconv.FormatInt(*b, 10)
}
