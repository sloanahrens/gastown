package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

func init() {
	metricsCmd.AddCommand(metricsRunCmd)
}

var metricsRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run one town-metrics hour: snapshot, then analyse, under the single-flight lock",
	Long: `Takes <town>/.runtime/town-metrics.lock, then runs the snapshot and the
analysis, passing both receipts through. A run that finds the lock held by
another run prints a skipped line and exits 0, so the daemon records the hour
as skipped rather than as a failure. Both steps run whichever one fails, and the
command fails with the first failing step's error.

This is the one entry point the daemon's town-metrics plugin calls.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		town, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runMetricsHour(cmd.Context(), cmd.OutOrStdout(), town, metrics.AcquireTownLock)
	},
}

// lockAcquirer takes the town-metrics lock at path or reports it held. The
// command passes metrics.AcquireTownLock; a test passes one that reports the
// lock held, so the skip path needs no second process.
type lockAcquirer func(path string) (*metrics.TownLock, error)

// runMetricsHour is one hour of the plugin: the lock, then the two steps in
// order. The snapshot's failure is the one reported when both fail, because the
// hour's record is the snapshot.
func runMetricsHour(ctx context.Context, out io.Writer, town string, acquire lockAcquirer) error {
	lockPath := filepath.Join(town, ".runtime", "town-metrics.lock")
	lock, err := acquire(lockPath)
	if errors.Is(err, metrics.ErrLockHeld) {
		fmt.Fprintf(out, "[town-metrics] another run holds %s; the next cooldown retries\n", lockPath)
		fmt.Fprintln(out, "[plugin-result skipped]")
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			fmt.Fprintf(out, "[town-metrics] releasing %s: %v\n", lockPath, err)
		}
	}()

	snapshotErr := runMetricsSnapshot(ctx, out, town)
	analyseErr := runMetricsAnalyse(ctx, out, town)
	if snapshotErr != nil {
		return snapshotErr
	}
	return analyseErr
}
