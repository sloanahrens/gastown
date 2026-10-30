package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jev/targets"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	jevTargetsRun     string
	jevTargetsRanked  string
	jevTargetsHistory string
	jevTargetsRepo    string
	jevTargetsRef     string
)

var jevTargetsCmd = &cobra.Command{
	Use:   "review-targets",
	Short: "Score each file in a deep-review slice and log the answers",
	Long: `Ask Jev one battery about each file in a deep-review rotation slice, in the
slice's rank order, and append one line per file to
<town>/.runtime/jev/targets.jsonl.

--ranked names the ranked slice rows scripts/deep-review.sh writes: one
tab-separated row per file holding the tier, the last review date, the risk
score, the size in bytes and the path, highest risk first (the script passes
slice-rows.txt, the slice itself; ranked.txt fits too). At most 60 files are
asked about. --history names the script's history.txt, the output of
git log --name-only --format=@%s, which tells each file its recent commit
subjects.

For each file Jev answers four questions in one call: whether it probably holds
a defect, whether recent commits changed its logic rather than text or tests,
whether its behavior is under-tested, and a 0-2 review priority. The state
holds the file's path, tier, last review date, risk score, line count, its last
ten commit subjects and its exported function and type names (at most 40),
never the file body. The body is read from --repo at --ref (default the current
directory and HEAD) for the line count and the names.

The run is advisory: it never reorders or drops a file from the slice, and the
log is the only thing it writes. It stops for the day once the town's Jev spend
reaches patrols.jev_targets.max_usd_per_day (default 1) and needs
patrols.jev_targets.usd_per_billion_tokens to price that spend. scripts/
deep-review.sh calls it in slice mode only when patrols.jev_targets.enabled is
true, and ignores its exit status.

Examples:
  gt jev review-targets --run gt-abcde --ranked slice-rows.txt --history history.txt
  gt jev review-targets --run gt-abcde --ranked slice-rows.txt --history history.txt \
    --repo . --ref origin/main`,
	Args: cobra.NoArgs,
	RunE: runJevTargets,
}

func init() {
	f := jevTargetsCmd.Flags()
	f.StringVar(&jevTargetsRun, "run", "", "The deep-review run bead whose slice this is (required)")
	f.StringVar(&jevTargetsRanked, "ranked", "", "The ranked slice rows, in rank order (required)")
	f.StringVar(&jevTargetsHistory, "history", "", "The git log --name-only history for the slice (required)")
	f.StringVar(&jevTargetsRepo, "repo", ".", "The repository the slice was ranked in")
	f.StringVar(&jevTargetsRef, "ref", "HEAD", "The ref the slice was ranked at")
	jevCmd.AddCommand(jevTargetsCmd)
}

func runJevTargets(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	if jevTargetsRun == "" {
		return fmt.Errorf("--run is required: every line names the deep-review run bead")
	}
	if jevTargetsRanked == "" || jevTargetsHistory == "" {
		return fmt.Errorf("--ranked and --history are required")
	}
	return runJevTargetsPass(cmd.OutOrStdout(), cmd.ErrOrStderr(), townRoot, jevTargetsRequest())
}

// jevTargetsRequest is the run the command asks for, from its flags.
func jevTargetsRequest() targets.RunRequest {
	return targets.RunRequest{
		Run:         jevTargetsRun,
		RankedPath:  jevTargetsRanked,
		HistoryPath: jevTargetsHistory,
		Repo:        jevTargetsRepo,
		Ref:         jevTargetsRef,
	}
}

// runJevTargetsPass reads the slice and history, asks the battery and prints
// what it did.
func runJevTargetsPass(out, errw io.Writer, townRoot string, req targets.RunRequest) error {
	settings, err := jevTargetsSettings(townRoot)
	if err != nil {
		return err
	}
	settings.Repo, settings.Ref = req.Repo, req.Ref
	pass, err := targets.Open(townRoot, settings, errw)
	if err != nil {
		return err
	}
	sum, err := pass.RunFile(context.Background(), req)
	if err != nil {
		return err
	}
	return reportJevTargets(out, errw, sum)
}

// reportJevTargets prints what a run did, names each skipped file on errw and
// fails a run that skipped every file it reached, so a caller checking the
// exit status does not read a run that logged nothing as a success.
func reportJevTargets(out, errw io.Writer, sum targets.Summary) error {
	for _, sk := range sum.SkippedFiles {
		fmt.Fprintf(errw, "jev review-targets: skipped %s: %s\n", sk.Path, sk.Reason)
	}
	switch {
	case sum.Files == 0:
		fmt.Fprintln(out, "jev review-targets: the slice is empty; nothing asked")
	case sum.CapReached:
		fmt.Fprintf(out, "jev review-targets: asked %d files; the daily cap was reached, the other %d were not asked\n",
			sum.Asked, sum.Files-sum.Asked-sum.Skipped)
	default:
		fmt.Fprintf(out, "jev review-targets: asked %d files, skipped %d\n", sum.Asked, sum.Skipped)
	}
	if sum.Asked == 0 && sum.Skipped > 0 {
		return fmt.Errorf("every file was skipped (%d); the log gained no line", sum.Skipped)
	}
	return nil
}

// jevTargetsSettings reads patrols.jev_targets from the patrol config. An
// absent block gives zero settings, which Open rejects for want of a price.
func jevTargetsSettings(townRoot string) (targets.Settings, error) {
	cfg, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return targets.Settings{}, fmt.Errorf("reading the patrol config: %w", err)
	}
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.JevTargets == nil {
		return targets.Settings{}, nil
	}
	c := cfg.Patrols.JevTargets
	return targets.Settings{
		MaxUSDPerDay:        c.MaxUSDPerDay,
		USDPerBillionTokens: c.USDPerBillionTokens,
		Scrub:               c.Scrub,
		ScrubCheck:          c.ScrubCheck,
	}, nil
}
