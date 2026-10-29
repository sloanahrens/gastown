package cmd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

func init() {
	metricsCmd.AddCommand(metricsAnalyseCmd)
}

var metricsAnalyseCmd = &cobra.Command{
	Use:   "analyse",
	Short: "Rebuild the town-metrics panel when the last analysis is a day old",
	Long: `Runs the daily analysis when its stamp in <town>/.runtime/metrics/latest is
older than a day: rebuilding the mango binary if its origin/main moved, running
the extract, joining the landings to the snapshots and the sandbox peaks, and
running the mango stage over the three targets. The panel, the joined landings
and the stamp are published into <town>/.runtime/metrics/latest.

A run that is not due does nothing and exits 0, so the hourly plugin can call it
beside the snapshot. A step that fails leaves the old panel and the old stamp in
place.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		town, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runMetricsAnalyse(cmd.Context(), cmd.OutOrStdout(), metrics.OSEnv(town), analysisOptions(town))
	},
}

// analysisOptions is the town's own mango repository and the binary built from
// it, for the two callers that wire the real environment.
func analysisOptions(town string) metrics.AnalysisOptions {
	return metrics.AnalysisOptions{
		MangoGitDir: filepath.Join(town, "mango", ".repo.git"),
		MangoBin:    filepath.Join(town, ".runtime", "metrics", "bin", "mango"),
	}
}

// runMetricsAnalyse runs the analysis in env and prints one receipt line: what
// it published, or that the last run is still current.
//
// Nothing here needs the gt source checkout: the extract runs in process inside
// the town (gt-fsoh3), so a town whose gastown/ holds no cmd/gt/main.go still
// analyses. The env is a parameter because the lookup that failed such a town
// lived in this function, and the regression test drives it with a fake
// (gt-w4brv).
func runMetricsAnalyse(ctx context.Context, out io.Writer, env metrics.Env, opts metrics.AnalysisOptions) error {
	res, err := metrics.RunAnalysis(ctx, env, opts)
	if err != nil {
		return err
	}
	if !res.Ran {
		_, err := fmt.Fprintf(out, "analyse: not due, the last analysis is under %s old\n", metrics.DailyEvery)
		return err
	}
	_, err = fmt.Fprintf(out, "analyse %s: %d landings, %d targets -> %s\n",
		res.Panel.GeneratedAt, res.Landings, len(res.Panel.Targets), filepath.Join(res.Dir, "panel.json"))
	if err == nil && res.SnapshotsSkipped > 0 {
		_, err = fmt.Fprintf(out, "analyse: %d snapshot(s) skipped, their ts does not parse\n", res.SnapshotsSkipped)
	}
	if err != nil || len(res.PeaksSkipped) == 0 {
		return err
	}
	_, err = fmt.Fprintf(out, "analyse: %d peak file(s) skipped, their rows are not joined: %s\n",
		len(res.PeaksSkipped), strings.Join(res.PeaksSkipped, ", "))
	return err
}
