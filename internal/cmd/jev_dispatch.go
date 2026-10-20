package cmd

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/workspace"
)

var jevDispatchRecent int

var jevDispatchCmd = &cobra.Command{
	Use:   "dispatch",
	Short: "Predict how each bead just placed on a seat will land, and log every prediction",
	Long: `Run one pass of the dispatch battery over the beads placed on a seat since
the last pass: a bead that is in_progress or hooked and whose description carries
an attached_at stamp later than the pass's cursor. For each one, Jev is asked
whether a polecat will land it on its first merge attempt, whether one worker
finishes it in twenty minutes, whether every acceptance item can be run or shown
in the worktree, whether it needs a reasoning model, whether it touches the
daemon, and how big it is. Every bead gets one line in
<town>/.runtime/jev/dispatch.jsonl.

The pass is advisory: it never edits, labels or comments on a bead, and it is
not part of the spec dispatcher's tick or of a sling. It stops for the day once
the town's Jev spend, every caller counted, reaches
patrols.jev_dispatch.max_usd_per_day (default 1), and it needs
patrols.jev_dispatch.usd_per_billion_tokens to price that spend. The config keys
are read whether or not the daemon's patrol is enabled, so a manual pass works
before the patrol is switched on.

The first pass only starts the cursor at its own time, so it asks about no bead
that was placed before it.

--recent N prints the last N lines of the log instead of running a pass.

Examples:
  gt jev dispatch
  gt jev dispatch --recent 20`,
	Args: cobra.NoArgs,
	RunE: runJevDispatch,
}

func init() {
	jevDispatchCmd.Flags().IntVar(&jevDispatchRecent, "recent", 0,
		"Print the last N lines of the dispatch log instead of running a pass")
	jevCmd.AddCommand(jevDispatchCmd)
}

func runJevDispatch(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	if cmd.Flags().Changed("recent") {
		if jevDispatchRecent < 1 {
			return fmt.Errorf("--recent must be at least 1, not %d", jevDispatchRecent)
		}
		return printRecentDispatch(cmd.OutOrStdout(), dispatch.LogPath(townRoot), jevDispatchRecent)
	}
	return runJevDispatchPass(cmd.OutOrStdout(), cmd.ErrOrStderr(), townRoot)
}

// runJevDispatchPass runs one pass with the settings from the patrol config
// and prints what it did.
func runJevDispatchPass(out, errw io.Writer, townRoot string) error {
	settings, err := jevDispatchSettings(townRoot)
	if err != nil {
		return err
	}
	pass, err := dispatch.Open(townRoot, settings, errw)
	if err != nil {
		return err
	}
	sum, err := pass.Run(context.Background())
	if err != nil {
		return err
	}
	switch {
	case sum.Started:
		fmt.Fprintln(out, "jev dispatch: first pass; cursor set to now, no beads asked")
	case sum.CapReached:
		fmt.Fprintf(out, "jev dispatch: asked %d beads, %d failed; the daily cap was reached, the rest wait for the next pass\n", sum.Asked, sum.Failed)
	default:
		fmt.Fprintf(out, "jev dispatch: asked %d beads, %d failed\n", sum.Asked, sum.Failed)
	}
	return nil
}

// jevDispatchSettings reads patrols.jev_dispatch from the patrol config. An
// absent block gives zero settings, which Open rejects for want of a price.
func jevDispatchSettings(townRoot string) (dispatch.Settings, error) {
	cfg, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return dispatch.Settings{}, fmt.Errorf("reading the patrol config: %w", err)
	}
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.JevDispatch == nil {
		return dispatch.Settings{}, nil
	}
	c := cfg.Patrols.JevDispatch
	return dispatch.Settings{
		MaxUSDPerDay:        c.MaxUSDPerDay,
		USDPerBillionTokens: c.USDPerBillionTokens,
	}, nil
}

// printRecentDispatch prints the last n log lines, oldest first, one per row.
func printRecentDispatch(w io.Writer, path string, n int) error {
	lines, err := dispatch.ReadRecent(path, n)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Fprintln(w, "no predictions logged yet")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "TIME\tBEAD\tAGENT\tLANDS\tFITS\tTICKABLE\tSTRONG\tDAEMON\tSIZE\tMODEL\n")
	for _, l := range lines {
		agent := l.Agent
		if agent == "" {
			agent = "-"
		}
		a := l.Answers
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.1f\t%s\n",
			l.TS.Local().Format("2006-01-02 15:04"), l.Bead, agent,
			a.LandsFirstTry, a.FitsTwentyMinutes, a.AcceptanceTickable, a.NeedsStrongModel, a.TouchesDaemon,
			a.Size.Score, l.Model)
	}
	return tw.Flush()
}
