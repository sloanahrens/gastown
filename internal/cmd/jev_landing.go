package cmd

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jev/landing"
	"github.com/steveyegge/gastown/internal/workspace"
)

var jevLandingRecent int

var jevLandingCmd = &cobra.Command{
	Use:   "landing",
	Short: "Ask Jev a second opinion on each risk-path landing, and log every answer",
	Long: `Run one pass of the landing battery over the gastown landings that touched
risk paths since the last pass. For each one, Jev answers five questions about
the landing's bead title and goal, risk paths, om verdict and score, gate
result, agent and the changed files with their line counts (never diff text):
whether the overseer should read the diff now, whether it touches town
machinery, whether it goes beyond the goal, whether tests cover it, and a 0-3
risk score. Every landing gets one line in <town>/.runtime/jev/landing.jsonl.

The pass is advisory: it never comments on a bead or changes what gt attention
shows. It stops for the day once the town's Jev spend reaches
patrols.jev_landing.max_usd_per_day (default 1), and it needs
patrols.jev_landing.usd_per_billion_tokens to price that spend. The config keys
are read whether or not the daemon's patrol is enabled, so a manual pass works
before the patrol is switched on.

The first pass only starts the cursor at its own time, so it asks about no
landing that landed before it.

--recent N prints the last N lines of the log instead of running a pass.

Examples:
  gt jev landing
  gt jev landing --recent 5`,
	Args: cobra.NoArgs,
	RunE: runJevLanding,
}

func init() {
	jevLandingCmd.Flags().IntVar(&jevLandingRecent, "recent", 0,
		"Print the last N lines of the landing log instead of running a pass")
	jevCmd.AddCommand(jevLandingCmd)
}

func runJevLanding(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	if cmd.Flags().Changed("recent") {
		if jevLandingRecent < 1 {
			return fmt.Errorf("--recent must be at least 1, not %d", jevLandingRecent)
		}
		return printRecentLanding(cmd.OutOrStdout(), landing.LogPath(townRoot), jevLandingRecent)
	}
	return runJevLandingPass(cmd.OutOrStdout(), cmd.ErrOrStderr(), townRoot)
}

// runJevLandingPass runs one pass with the settings from the patrol config and
// prints what it did.
func runJevLandingPass(out, errw io.Writer, townRoot string) error {
	settings, err := jevLandingSettings(townRoot)
	if err != nil {
		return err
	}
	pass, err := landing.Open(townRoot, settings, errw)
	if err != nil {
		return err
	}
	sum, err := pass.Run(context.Background())
	if err != nil {
		return err
	}
	switch {
	case sum.Started:
		fmt.Fprintln(out, "jev landing: first pass; cursor set to now, no landings asked")
	case sum.CapReached:
		fmt.Fprintf(out, "jev landing: asked %d landings; the daily cap was reached, the rest wait for the next pass\n", sum.Asked)
	default:
		fmt.Fprintf(out, "jev landing: asked %d landings, skipped %d\n", sum.Asked, sum.Skipped)
	}
	return nil
}

// jevLandingSettings reads patrols.jev_landing from the patrol config. An
// absent block gives zero settings, which Open rejects for want of a price.
func jevLandingSettings(townRoot string) (landing.Settings, error) {
	cfg, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return landing.Settings{}, fmt.Errorf("reading the patrol config: %w", err)
	}
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.JevLanding == nil {
		return landing.Settings{}, nil
	}
	c := cfg.Patrols.JevLanding
	return landing.Settings{
		MaxUSDPerDay:        c.MaxUSDPerDay,
		USDPerBillionTokens: c.USDPerBillionTokens,
		Scrub:               c.Scrub,
		ScrubCheck:          c.ScrubCheck,
	}, nil
}

// printRecentLanding prints the last n log lines, oldest first, one per row:
// the noul probabilities, then the risk level.
func printRecentLanding(w io.Writer, path string, n int) error {
	lines, err := landing.ReadRecent(path, n)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Fprintln(w, "no landings logged yet")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "TIME\tBEAD\tAGENT\tRISK\tREVIEW-NOW\tMACHINERY\tBEYOND-GOAL\tTESTS\tVERSION\n")
	for _, l := range lines {
		agent := l.Agent
		if agent == "" {
			agent = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\tv%d %s\n",
			l.TS.UTC().Format("2006-01-02 15:04Z"), l.Bead, agent,
			riskCell(l.Answers[landing.QRisk].Score, l.Answers[landing.QRisk].Confidence),
			noulCell(l.Answers[landing.QNeedsReviewNow].Noul),
			noulCell(l.Answers[landing.QTouchesTownMachinery].Noul),
			noulCell(l.Answers[landing.QBeyondGoal].Noul),
			noulCell(l.Answers[landing.QTestsCoverChange].Noul),
			l.QuestionVersion, l.Model)
	}
	return tw.Flush()
}

// noulCell is a noul answer's probability, or "-" when the line has none.
func noulCell(noul *float64) string {
	if noul == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *noul)
}

// riskCell is the risk score and Jev's confidence in it, or "-".
func riskCell(score, confidence *float64) string {
	if score == nil {
		return "-"
	}
	if confidence == nil {
		return fmt.Sprintf("%.0f", *score)
	}
	return fmt.Sprintf("%.0f (%.2f)", *score, *confidence)
}
