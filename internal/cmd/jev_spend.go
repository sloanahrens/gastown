package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/workspace"
)

var jevCmd = &cobra.Command{
	Use:     "jev",
	GroupID: GroupDiag,
	Short:   "Read Jev's usage ledger and report battery health",
	Long: `Read the town's Jev logs under <town>/.runtime/jev: the usage ledger,
where every answered Jev call appends its token counts and the caller that
spent them, and the request log, where every call that reaches the network
appends what it sent as sizes and a hash. The calls record themselves; gt jev
only reads. The health subcommand judges the batteries from the files they
write; it reads nothing else. The subcommands that read these logs run in the
daemon pod.`,
	RunE: requireSubcommand,
}

// jevSpendDeps is what the spend subcommand takes from the host: the town whose
// ledger it reads and the environment lookup that decides whether the session
// is in the daemon pod. A test supplies both.
type jevSpendDeps struct {
	findTownRoot func() (string, error)
	getenv       func(string) string
}

// realJevSpendDeps is jevSpendDeps on this host.
func realJevSpendDeps() jevSpendDeps {
	return jevSpendDeps{findTownRoot: workspace.FindFromCwdOrError, getenv: os.Getenv}
}

// jevSpendFlags is one spend command's flags; a command owns its own so a later
// run never inherits an earlier run's values.
type jevSpendFlags struct {
	asJSON        bool
	hours         int
	usdPerBillion float64
}

// newJevSpendCmd builds `gt jev spend`.
func newJevSpendCmd(d jevSpendDeps) *cobra.Command {
	var f jevSpendFlags
	cmd := &cobra.Command{
		Use:   "spend",
		Short: "Print Jev spend for a window, by caller",
		Long: `Sum the ledger lines stamped in the --hours window ending now and price
their tokens at --usd-per-billion-tokens, which is required because the
ledger stores tokens and never dollars. A missing ledger prints zeros; a line
that does not parse is skipped and counted in lines_skipped.

The first line names the copy the numbers came from: the newest write among the
enabled batteries' logs and the ledger. It warns when that write is older than a
live copy may be, or when the copy cannot be read; the numbers are printed either way.

--json prints {calls, input_tokens, output_tokens, usd, lines_skipped,
by_caller, periods, copy_newest_write, copy_stale, copy_warning}, where by_caller
maps each caller to its {calls, usd}, periods holds the dollars of the last hour,
today (since local midnight), yesterday and the last 168 hours, for a dashboard
that draws Jev spend beside DeepSeek's and Claude's. copy_newest_write is {at,
path} of the copy's newest write, null when nothing has been written.
copy_warning is set only when the copy could not be read, naming why.
` + jevPodHelp + `

Examples:
  gt jev spend --usd-per-billion-tokens 3 --json
  gt jev spend --hours 168 --usd-per-billion-tokens 3`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return jevSpendRun(cmd, d, f)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.asJSON, "json", false, "Print the report as JSON")
	fl.IntVar(&f.hours, "hours", 24, "Length of the window ending now, in hours")
	fl.Float64Var(&f.usdPerBillion, "usd-per-billion-tokens", 0,
		"Price of input and output tokens, in US dollars per billion (required: the ledger stores tokens, not dollars)")
	return cmd
}

var jevSpendCmd = newJevSpendCmd(realJevSpendDeps())

func init() {
	jevCmd.AddCommand(jevSpendCmd)
	rootCmd.AddCommand(jevCmd)
}

// jevSpendRun refuses outside the daemon pod, whose logs the command reads,
// then finds the town and reports its ledger.
func jevSpendRun(cmd *cobra.Command, d jevSpendDeps, f jevSpendFlags) error {
	if err := jevRequirePod(d.getenv, cmd); err != nil {
		return err
	}
	townRoot, err := d.findTownRoot()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return jevSpendWithFlags(cmd.OutOrStdout(), townRoot, f.hours, f.usdPerBillion, f.asJSON, time.Now())
}

// jevSpendWithFlags validates the window and the price, then reports the
// ledger's spend over the window ending at now.
func jevSpendWithFlags(w io.Writer, townRoot string, hours int, usdPerBillion float64, asJSON bool, now time.Time) error {
	if hours < 1 {
		return fmt.Errorf("--hours must be at least 1, not %d", hours)
	}
	if !(usdPerBillion > 0 && !math.IsInf(usdPerBillion, 0) && !math.IsNaN(usdPerBillion)) {
		return fmt.Errorf("--usd-per-billion-tokens is required and must be finite and above zero, not %v", usdPerBillion)
	}
	from := now.Add(-time.Duration(hours) * time.Hour)
	rep, err := jev.ReadSpend(jev.UsageLedgerPath(townRoot), from, now, usdPerBillion)
	if err != nil {
		return err
	}
	note := jevCopyAge(townRoot, now, os.Stat, "")
	if asJSON {
		// The periods ride beside the window's own fields, for the dashboard's
		// Jev row (today, yesterday, 7 days, last hour); a reader that does not
		// know them ignores them.
		periods, err := jev.ReadSpendPeriods(jev.UsageLedgerPath(townRoot), now, usdPerBillion)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(jevSpendDoc{
			SpendReport:     rep,
			Periods:         periods,
			CopyNewestWrite: note.Doc,
			CopyStale:       note.Stale,
			CopyWarning:     note.Warning,
		})
	}
	if _, err := fmt.Fprintln(w, note.Line); err != nil {
		return err
	}
	return writeJevSpendTable(w, rep)
}

// jevSpendDoc is the --json document: the window's report, the periods the
// dashboard draws, and the copy the ledger was read from, so a reader can tell
// which copy the numbers describe.
type jevSpendDoc struct {
	jev.SpendReport
	Periods         jev.SpendPeriods `json:"periods"`
	CopyNewestWrite *jevCopyWrite    `json:"copy_newest_write"`
	CopyStale       bool             `json:"copy_stale"`
	// CopyWarning names why the copy could not be read; absent when it was.
	CopyWarning string `json:"copy_warning,omitempty"`
}

// writeJevSpendTable prints the totals and then one row per caller, in name
// order, so two runs compare line by line.
func writeJevSpendTable(w io.Writer, rep jev.SpendReport) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(tw, "Calls\tInput tokens\tOutput tokens\tUSD\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "%d\t%d\t%d\t%.6f\n", rep.Calls, rep.InputTokens, rep.OutputTokens, rep.USD); err != nil {
		return err
	}
	callers := make([]string, 0, len(rep.ByCaller))
	for name := range rep.ByCaller {
		callers = append(callers, name)
	}
	sort.Strings(callers)
	if len(callers) > 0 {
		if _, err := fmt.Fprintf(tw, "\nCaller\tCalls\tUSD\n"); err != nil {
			return err
		}
		for _, name := range callers {
			c := rep.ByCaller[name]
			if _, err := fmt.Fprintf(tw, "%s\t%d\t%.6f\n", name, c.Calls, c.USD); err != nil {
				return err
			}
		}
	}
	if rep.LinesSkipped > 0 {
		if _, err := fmt.Fprintf(tw, "\nlines skipped: %d\n", rep.LinesSkipped); err != nil {
			return err
		}
	}
	return tw.Flush()
}
