package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

func init() {
	metricsCmd.AddCommand(metricsSnapshotCmd)
}

var metricsSnapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Collect one town snapshot and append it to the metrics log",
	Long: `Collects one snapshot with the registered sources and appends it to
<town>/.runtime/metrics/snapshots.jsonl, then prints one receipt line. A source
that fails or times out leaves its group null and an errors entry in the record;
only a failed append fails the command.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		town, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runMetricsSnapshot(cmd.Context(), cmd.OutOrStdout(), town)
	},
}

// runMetricsSnapshot takes one snapshot of town and prints the receipt.
func runMetricsSnapshot(ctx context.Context, out io.Writer, town string) error {
	rec, path, err := metrics.RunSnapshot(ctx, metrics.OSEnv(town), metrics.Sources(), metrics.SourceTimeout)
	if err != nil {
		return err
	}
	// The placement group is written under the record's fixed placement key,
	// not in Groups.
	groups := len(rec.Groups)
	if rec.Placement != nil {
		groups++
	}
	_, err = fmt.Fprintf(out, "snapshot %s appended to %s (%d groups, %d errors)\n", rec.TS, path, groups, len(rec.Errors))
	return err
}
