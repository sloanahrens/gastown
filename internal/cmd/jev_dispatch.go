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
	Long: `Ask Jev the dispatch battery about the beads placed on a seat since the last
pass, and append one prediction per bead to the town's dispatch log. Advisory
and log-only: it never edits, labels or comments on a bead. A question that
fails is asked again on the next pass until it has failed three times.

--recent N prints the last N lines of the log instead of running a pass.

The state, the log line, the cursor and the patrol config keys are in
docs/reference.md under "Dispatch battery".`,
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
	for _, f := range sum.Failures {
		if f.Final {
			fmt.Fprintf(errw, "jev dispatch: %s failed %d times, giving up: %v\n", f.Bead, f.Attempts, f.Err)
			continue
		}
		fmt.Fprintf(errw, "jev dispatch: %s failed, retrying next pass: %v\n", f.Bead, f.Err)
	}
	if sum.Started {
		fmt.Fprintln(out, "jev dispatch: first pass; cursor set to now, no beads asked")
		return nil
	}
	msg := fmt.Sprintf("jev dispatch: asked %d beads, %d failed", sum.Asked, sum.Failed)
	if sum.GivenUp > 0 {
		msg += fmt.Sprintf(", %d given up", sum.GivenUp)
	}
	if sum.CapReached {
		msg += "; the daily cap was reached, the rest wait for the next pass"
	}
	fmt.Fprintln(out, msg)
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
		Scrub:               c.Scrub,
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.2f\t%.2f\t%.2f\t%.2f\t%.1f\t%s\n",
			l.TS.Local().Format("2006-01-02 15:04"), l.Bead, agent,
			noulCell(a.LandsFirstTry), a.FitsTwentyMinutes, a.AcceptanceTickable, a.NeedsStrongModel, a.TouchesDaemon,
			a.Size.Score, l.Model)
	}
	return tw.Flush()
}
