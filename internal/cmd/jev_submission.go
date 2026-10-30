package cmd

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/workspace"
)

var jevSubmissionRecent int

var jevSubmissionCmd = &cobra.Command{
	Use:   "submission",
	Short: "Ask Jev about each branch submitted for landing, and log every answer",
	Long: `Run one pass of the submission battery over the gastown work beads submitted
for landing since the last pass. For each one, Jev answers ten questions about
the bead's title, Goal, Acceptance and MR notes, the commit subjects on
target..head and the changed paths with their line counts (never diff text):
whether tests cover the change, whether it goes beyond the goal, whether the
commit subjects match the change, whether it touches town machinery, whether a
careful reviewer would approve it, whether the MR notes' verification was run
and applies to the boxes, whether the ticked boxes are credible, how the work
ended, and a 0-3 risk score. Every submission gets one line in
<town>/.runtime/jev/submission.jsonl.

The commits and the diff stat come from a bare clone the pass owns at
<town>/.runtime/jev/repos/gastown.git, created on its first run from the rig's
origin and fetched by every pass. The pass never runs git in a town worktree.
A fetch that fails skips the pass and leaves its cursor where it was.

The pass is advisory: it never comments on a bead, blocks or delays a landing,
or writes under .runtime/landings. It stops for the day once the town's Jev
spend reaches patrols.jev_submission.max_usd_per_day (default 1), and it needs
patrols.jev_submission.usd_per_billion_tokens to price that spend. The config
keys are read whether or not the daemon's patrol is enabled, so a manual pass
works before the patrol is switched on.

The first pass only starts the cursor at its own time, so it asks about no
submission already waiting.

--recent N prints the last N lines of the log instead of running a pass.

Examples:
  gt jev submission
  gt jev submission --recent 5`,
	Args: cobra.NoArgs,
	RunE: runJevSubmission,
}

func init() {
	jevSubmissionCmd.Flags().IntVar(&jevSubmissionRecent, "recent", 0,
		"Print the last N lines of the submission log instead of running a pass")
	jevCmd.AddCommand(jevSubmissionCmd)
}

func runJevSubmission(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	if cmd.Flags().Changed("recent") {
		if jevSubmissionRecent < 1 {
			return fmt.Errorf("--recent must be at least 1, not %d", jevSubmissionRecent)
		}
		return printRecentSubmission(cmd.OutOrStdout(), submission.LogPath(townRoot), jevSubmissionRecent)
	}
	return runJevSubmissionPass(cmd.OutOrStdout(), cmd.ErrOrStderr(), townRoot)
}

// runJevSubmissionPass runs one pass with the settings from the patrol config
// and prints what it did.
func runJevSubmissionPass(out, errw io.Writer, townRoot string) error {
	settings, err := jevSubmissionSettings(townRoot)
	if err != nil {
		return err
	}
	pass, err := submission.Open(townRoot, settings, errw)
	if err != nil {
		return err
	}
	sum, err := pass.Run(context.Background())
	if err != nil {
		return err
	}
	switch {
	case sum.Started:
		fmt.Fprintln(out, "jev submission: first pass; cursor set to now, no submissions asked")
	case sum.CapReached:
		fmt.Fprintf(out, "jev submission: asked %d submissions, repeated %d; the daily cap was reached, the rest wait for the next pass\n", sum.Asked, sum.Repeated)
	default:
		fmt.Fprintf(out, "jev submission: asked %d submissions, skipped %d, repeated %d\n", sum.Asked, sum.Skipped, sum.Repeated)
	}
	return nil
}

// jevSubmissionSettings reads patrols.jev_submission from the patrol config.
// An absent block gives zero settings, which Open rejects for want of a price.
func jevSubmissionSettings(townRoot string) (submission.Settings, error) {
	cfg, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return submission.Settings{}, fmt.Errorf("reading the patrol config: %w", err)
	}
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.JevSubmission == nil {
		return submission.Settings{}, nil
	}
	c := cfg.Patrols.JevSubmission
	return submission.Settings{
		MaxUSDPerDay:        c.MaxUSDPerDay,
		USDPerBillionTokens: c.USDPerBillionTokens,
		Scrub:               c.Scrub,
	}, nil
}

// printRecentSubmission prints the last n log lines, oldest first, one per
// row: the noul probabilities, then the risk level.
func printRecentSubmission(w io.Writer, path string, n int) error {
	lines, err := submission.ReadRecent(path, n)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Fprintln(w, "no submissions logged yet")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "TIME\tBEAD\tBRANCH\tTARGET\tRISK\tREVIEW\tTESTS\tBEYOND-GOAL\tMATCH\tMACHINERY\tVERSION\n")
	for _, l := range lines {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\tv%d %s\n",
			l.TS.UTC().Format("2006-01-02 15:04Z"), l.Bead, l.Branch, l.Target,
			riskCell(l.Answers[submission.QRisk].Score, l.Answers[submission.QRisk].Confidence),
			noulCell(l.Answers[submission.QWillPassReview].Noul),
			noulCell(l.Answers[submission.QTestsCoverChange].Noul),
			noulCell(l.Answers[submission.QBeyondGoal].Noul),
			noulCell(l.Answers[submission.QCommitsMatchChange].Noul),
			noulCell(l.Answers[submission.QTouchesTownMachinery].Noul),
			l.QuestionVersion, l.Model)
	}
	return tw.Flush()
}
