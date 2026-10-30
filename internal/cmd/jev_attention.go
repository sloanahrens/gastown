package cmd

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/daemon"
	jevattention "github.com/steveyegge/gastown/internal/jev/attention"
	"github.com/steveyegge/gastown/internal/workspace"
)

var jevAttentionRecent int

var jevAttentionCmd = &cobra.Command{
	Use:   "attention",
	Short: "Ask Jev the attention question set about each new item, and log every answer",
	Long: `Run one pass of the attention battery over the items raised since the last
pass. For each one, Jev is asked four statements, each as the probability that
it holds (a person has to act, it clears on its own, a routine action fixes it,
another open item repeats it), and one urgency score, in one call. Every item
gets one line in
<town>/.runtime/jev/attention.jsonl, whatever Jev answers.

The pass is advisory and log-only: it never acks, hides, wakes or changes an
item, and it writes nothing under .runtime/attention. It stops for the day once
its Jev spend reaches patrols.jev_attention.max_usd_per_day (default 1), and it
needs patrols.jev_attention.usd_per_billion_tokens to price that spend. The
config keys are read whether or not the daemon's patrol is enabled, so a manual
pass works before the patrol is switched on.

The first pass only starts the cursor at its own time, so it asks about no
item that was raised before it.

--recent N prints the last N lines of the log instead of running a pass.

Examples:
  gt jev attention
  gt jev attention --recent 20`,
	Args: cobra.NoArgs,
	RunE: runJevAttention,
}

func init() {
	jevAttentionCmd.Flags().IntVar(&jevAttentionRecent, "recent", 0,
		"Print the last N lines of the answer log instead of running a pass")
	jevCmd.AddCommand(jevAttentionCmd)
}

func runJevAttention(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	if cmd.Flags().Changed("recent") {
		if jevAttentionRecent < 1 {
			return fmt.Errorf("--recent must be at least 1, not %d", jevAttentionRecent)
		}
		return printRecentAttention(cmd.OutOrStdout(), jevattention.LogPath(townRoot), jevAttentionRecent)
	}
	return runJevAttentionPass(cmd.OutOrStdout(), cmd.ErrOrStderr(), townRoot)
}

// runJevAttentionPass runs one pass with the settings from the patrol config
// and prints what it did.
func runJevAttentionPass(out, errw io.Writer, townRoot string) error {
	settings, err := jevAttentionSettings(townRoot)
	if err != nil {
		return err
	}
	pass, err := jevattention.Open(townRoot, settings, errw)
	if err != nil {
		return err
	}
	sum, err := pass.Run(context.Background())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "jev attention: %s\n", sum.Report())
	return nil
}

// jevAttentionSettings reads patrols.jev_attention from the patrol config. An
// absent block gives zero settings, which Open rejects for want of a price.
func jevAttentionSettings(townRoot string) (jevattention.Settings, error) {
	cfg, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return jevattention.Settings{}, fmt.Errorf("reading the patrol config: %w", err)
	}
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.JevAttention == nil {
		return jevattention.Settings{}, nil
	}
	c := cfg.Patrols.JevAttention
	return jevattention.Settings{
		MaxUSDPerDay:        c.MaxUSDPerDay,
		USDPerBillionTokens: c.USDPerBillionTokens,
		Scrub:               c.Scrub,
		ScrubCheck:          c.ScrubCheck,
	}, nil
}

// printRecentAttention prints the last n log lines, oldest first, one per row.
func printRecentAttention(w io.Writer, path string, n int) error {
	lines, err := jevattention.ReadRecent(path, n)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Fprintln(w, "no answers logged yet")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "TIME\tKEY\tKIND\tSEVERITY\tNEEDS-HUMAN\tCLEARS\tROUTINE\tREPEATS\tURGENCY\tMODEL\n")
	for _, l := range lines {
		needs, clears, routine, repeats, urgency := "-", "-", "-", "-", "-"
		if a := l.Answers; a != nil {
			needs = fmt.Sprintf("%.2f", a.NeedsHuman)
			clears = fmt.Sprintf("%.2f", a.ClearsOnItsOwn)
			routine = noulCell(a.RoutineActionFixes)
			repeats = fmt.Sprintf("%.2f", a.RepeatsOpenItem)
			urgency = fmt.Sprintf("%.0f (%.2f)", a.Urgency.Score, a.Urgency.Confidence)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			l.TS.Local().Format("2006-01-02 15:04"), l.Key, l.Kind, l.Severity,
			needs, clears, routine, repeats, urgency, l.Model)
	}
	return tw.Flush()
}
