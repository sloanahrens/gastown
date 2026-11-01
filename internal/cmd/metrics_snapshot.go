package cmd

import (
	"context"
	"errors"
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
	Long: `Collects one snapshot with the registered sources, appends it to
<town>/.runtime/metrics/snapshots.jsonl and prints its receipt, then appends the
local day's per-rig bd lines to <town>/.runtime/metrics/bd-daily.jsonl and prints
their receipt. A source that fails or times out leaves its group null and an
errors entry in the record; a failed append, or a day the daily step cannot
read, fails the command.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		town, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runMetricsSnapshot(cmd.Context(), cmd.OutOrStdout(), town)
	},
}

// runMetricsSnapshot takes one snapshot of town with the real sources and the
// real environment and appends the day's per-rig bd lines. It is the only place
// the command wires the real registry and OSEnv.
func runMetricsSnapshot(ctx context.Context, out io.Writer, town string) error {
	return runMetricsSteps(ctx, out, metrics.OSEnv(town), metrics.Sources())
}

// runMetricsSteps is the hour's two steps over env: the snapshot record, then
// the day's per-rig lines. They are independent, so a failed snapshot append
// does not cost the day its line, and the error names every step that failed.
func runMetricsSteps(ctx context.Context, out io.Writer, env metrics.Env, sources []metrics.Source) error {
	snapshotErr := runMetricsSnapshotWith(ctx, out, env, sources)
	return errors.Join(snapshotErr, runBdDaily(ctx, out, env))
}

// runBdDaily appends the day's per-rig bd lines and prints their receipt. It
// runs after the snapshot record, so an hour whose snapshot append failed still
// leaves the next run to write the day's line.
func runBdDaily(ctx context.Context, out io.Writer, env metrics.Env) error {
	res, err := metrics.RunBdDaily(ctx, env)
	if err != nil {
		return fmt.Errorf("bd daily: %w", err)
	}
	_, err = fmt.Fprintf(out, "bd-daily %s appended to %s (%d rigs written, %d already recorded, %d gaps)\n",
		res.Date, res.Path, len(res.Written), len(res.Recorded), len(res.Gaps))
	return err
}

// runMetricsSnapshotWith collects one snapshot over the given sources in env,
// appends it to env.Town's snapshot log and prints the receipt.
func runMetricsSnapshotWith(ctx context.Context, out io.Writer, env metrics.Env, sources []metrics.Source) error {
	rec, path, err := metrics.RunSnapshot(ctx, env, sources, metrics.SourceTimeout)
	if err != nil {
		return err
	}
	// The placement group is written under the record's fixed placement key,
	// not in Groups.
	groups := len(rec.Groups)
	if rec.Placement != nil {
		groups++
	}
	// An all-stub or all-null collection leaves a record whose groups all
	// carry no columns; the receipt says so rather than let the append, which
	// succeeded, read as a snapshot of data (gt-pmovc).
	collected := len(rec.Placement) > 0
	for _, cols := range rec.Groups {
		if len(cols) > 0 {
			collected = true
			break
		}
	}
	note := ""
	if !collected {
		note = "; no data collected"
	}
	if rec.Placement == nil {
		note += "; placement: none known"
	}
	_, err = fmt.Fprintf(out, "snapshot %s appended to %s (%d groups, %d errors)%s\n", rec.TS, path, groups, len(rec.Errors), note)
	return err
}
