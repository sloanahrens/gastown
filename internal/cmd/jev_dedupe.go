package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
	"github.com/steveyegge/gastown/internal/workspace"
)

var jevDedupeRecent int

var jevDedupeCmd = &cobra.Command{
	Use:   "dedupe",
	Short: "Suggest the bead each new finding duplicates, and log every suggestion",
	Long: `Run one pass of the duplicate check over the findings filed since the last
pass. A finding is a bead titled "Found:" or labeled review-deep or om-followup.
For each one, Jev is offered up to five candidate beads from hm query and is
asked which, if any, the finding duplicates. Every finding gets one line in
<town>/.runtime/jev/dedupe.jsonl, whatever Jev answers.

The pass is advisory: it never edits, labels, comments on, closes or links a
bead. It stops for the day once its Jev spend reaches
patrols.jev_dedupe.max_usd_per_day (default 1), and it needs
patrols.jev_dedupe.usd_per_billion_tokens to price that spend. The config keys
are read whether or not the daemon's patrol is enabled, so a manual pass works
before the patrol is switched on.

The first pass only starts the cursor at its own time, so it asks about no
finding that was filed before it.

--recent N prints the last N lines of the log instead of running a pass.

Examples:
  gt jev dedupe
  gt jev dedupe --recent 20`,
	Args: cobra.NoArgs,
	RunE: runJevDedupe,
}

func init() {
	jevDedupeCmd.Flags().IntVar(&jevDedupeRecent, "recent", 0,
		"Print the last N lines of the suggestion log instead of running a pass")
	jevCmd.AddCommand(jevDedupeCmd)
}

func runJevDedupe(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	if cmd.Flags().Changed("recent") {
		if jevDedupeRecent < 1 {
			return fmt.Errorf("--recent must be at least 1, not %d", jevDedupeRecent)
		}
		return printRecentDedupe(cmd.OutOrStdout(), dedupe.LogPath(townRoot), jevDedupeRecent)
	}
	return runJevDedupePass(cmd.OutOrStdout(), cmd.ErrOrStderr(), townRoot)
}

// runJevDedupePass runs one pass with the settings from the patrol config
// and prints what it did.
func runJevDedupePass(out, errw io.Writer, townRoot string) error {
	settings, err := jevDedupeSettings(townRoot)
	if err != nil {
		return err
	}
	pass, err := dedupe.Open(townRoot, settings, errw)
	if err != nil {
		return err
	}
	sum, err := pass.Run(context.Background())
	if err != nil {
		return err
	}
	switch {
	case sum.Started:
		fmt.Fprintln(out, "jev dedupe: first pass; cursor set to now, no findings asked")
	case sum.CapReached:
		fmt.Fprintf(out, "jev dedupe: asked %d findings; the daily cap was reached, the rest wait for the next pass\n", sum.Asked)
	default:
		fmt.Fprintf(out, "jev dedupe: asked %d findings\n", sum.Asked)
	}
	return nil
}

// jevDedupeSettings reads patrols.jev_dedupe from the patrol config. An absent
// block gives zero settings, which Open rejects for want of a price.
func jevDedupeSettings(townRoot string) (dedupe.Settings, error) {
	cfg, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return dedupe.Settings{}, fmt.Errorf("reading the patrol config: %w", err)
	}
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.JevDedupe == nil {
		return dedupe.Settings{}, nil
	}
	c := cfg.Patrols.JevDedupe
	return dedupe.Settings{
		MaxUSDPerDay:        c.MaxUSDPerDay,
		USDPerBillionTokens: c.USDPerBillionTokens,
	}, nil
}

// printRecentDedupe prints the last n log lines, oldest first, one per row.
func printRecentDedupe(w io.Writer, path string, n int) error {
	lines, err := dedupe.ReadRecent(path, n)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Fprintln(w, "no suggestions logged yet")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "TIME\tFINDING\tCANDIDATE\tCONFIDENCE\tOFFERED\n")
	for _, l := range lines {
		offered := "none"
		if len(l.CandidatesOffered) > 0 {
			offered = strings.Join(l.CandidatesOffered, ", ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.2f\t%s\n",
			l.TS.Local().Format("2006-01-02 15:04"), l.Finding, l.Candidate, l.Confidence, offered)
	}
	return tw.Flush()
}
