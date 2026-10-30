package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	jevSpendJSON       bool
	jevSpendHours      int
	jevSpendUSDPerBTok float64
)

var jevCmd = &cobra.Command{
	Use:     "jev",
	GroupID: GroupDiag,
	Short:   "Read Jev's usage ledger",
	Long: `Read the town's Jev logs under <town>/.runtime/jev: the usage ledger,
where every answered Jev call appends its token counts and the caller that
spent them, and the request log, where every call that reaches the network
appends what it sent as sizes and a hash. The calls record themselves; gt jev
only reads.`,
	RunE: requireSubcommand,
}

var jevSpendCmd = &cobra.Command{
	Use:   "spend",
	Short: "Print Jev spend for a window, by caller",
	Long: `Sum the ledger lines stamped in the --hours window ending now and price
their tokens at --usd-per-billion-tokens, which is required because the
ledger stores tokens and never dollars. A missing ledger prints zeros; a line
that does not parse is skipped and counted in lines_skipped.

--json prints {calls, input_tokens, output_tokens, usd, lines_skipped,
by_caller}, where by_caller maps each caller to its {calls, usd}, for a
dashboard that draws Jev spend beside DeepSeek's and Claude's.

Examples:
  gt jev spend --usd-per-billion-tokens 3 --json
  gt jev spend --hours 168 --usd-per-billion-tokens 3`,
	Args: cobra.NoArgs,
	RunE: runJevSpend,
}

func init() {
	f := jevSpendCmd.Flags()
	f.BoolVar(&jevSpendJSON, "json", false, "Print the report as JSON")
	f.IntVar(&jevSpendHours, "hours", 24, "Length of the window ending now, in hours")
	f.Float64Var(&jevSpendUSDPerBTok, "usd-per-billion-tokens", 0,
		"Price of input and output tokens, in US dollars per billion (required: the ledger stores tokens, not dollars)")
	jevCmd.AddCommand(jevSpendCmd)
	rootCmd.AddCommand(jevCmd)
}

func runJevSpend(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return jevSpendWithFlags(cmd.OutOrStdout(), townRoot, jevSpendHours, jevSpendUSDPerBTok, jevSpendJSON, time.Now())
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
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return writeJevSpendTable(w, rep)
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
