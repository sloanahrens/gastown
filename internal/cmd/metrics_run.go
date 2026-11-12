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
	Short: "Run one town-metrics hour: snapshot, install regime events, then analyse, under the single-flight lock",
	Long: `Takes <town>/.runtime/town-metrics.lock, then runs the snapshot, records the
install receipts the regime log does not hold, and runs the analysis, passing
each receipt through. A run that finds the lock held by another run prints a
skipped line and exits 0, so the daemon records the hour as skipped rather than
as a failure. Every step runs whichever one fails, and the command fails with
the first failing step's error.

This is the one entry point the daemon's town-metrics plugin calls.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		town, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runMetricsHour(cmd.Context(), cmd.OutOrStdout(), town)
	},
}

// townMetricsActor is who the hour records the install regime events under. It
// is the process, not a session: the hour runs in the pod with no operator and
// no session actor to name.
const townMetricsActor = "town-metrics"

// metricsHourSteps are the hour's three jobs. Production passes the functions
// below; a test passes fakes, so the hour's order and its error precedence are
// driven without the snapshot's probes or the analysis's commands.
type metricsHourSteps struct {
	snapshot func(context.Context, io.Writer, string) error
	regime   func(io.Writer, string) error
	analyse  func(context.Context, io.Writer, string) error
}

// hostMetricsHourSteps is the hour's three jobs over the town it runs in.
func hostMetricsHourSteps() metricsHourSteps {
	return metricsHourSteps{
		snapshot: runMetricsSnapshot,
		regime: func(out io.Writer, town string) error {
			return recordInstallRegimeEvents(out, metrics.OSEnv(town), townMetricsActor)
		},
		analyse: func(ctx context.Context, out io.Writer, town string) error {
			return runMetricsAnalyse(ctx, out, metrics.OSEnv(town), analysisOptions(town))
		},
	}
}

// runMetricsHour is one hour of the plugin: the lock, then the three steps in
// order. The snapshot's failure is the one reported when all fail, because the
// hour's record is the snapshot.
func runMetricsHour(ctx context.Context, out io.Writer, town string) error {
	return runMetricsHourWith(ctx, out, town, hostMetricsHourSteps())
}

// runMetricsHourWith is runMetricsHour over steps.
func runMetricsHourWith(ctx context.Context, out io.Writer, town string, steps metricsHourSteps) error {
	lockPath := filepath.Join(town, ".runtime", "town-metrics.lock")
	lock, err := metrics.AcquireTownLock(lockPath)
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

	snapshotErr := steps.snapshot(ctx, out, town)
	regimeErr := steps.regime(out, town)
	analyseErr := steps.analyse(ctx, out, town)
	if snapshotErr != nil {
		return snapshotErr
	}
	if regimeErr != nil {
		return regimeErr
	}
	return analyseErr
}

// recordInstallRegimeEvents appends one install regime event for every applied
// receipt the town's regime log does not hold, so an install is in the log the
// hour it lands without a person typing it. A receipt line the run cannot use
// prints one line and the other receipts still record. A receipts file the run
// cannot read fails the step, and the hour with it, after the other receipts are
// recorded. A record the log cannot take also fails the step, and it stops the
// step there.
func recordInstallRegimeEvents(out io.Writer, env metrics.Env, actor string) error {
	res, err := metrics.RecordInstallReceipts(env, actor, func(line string) {
		fmt.Fprintln(out, "[town-metrics] "+line)
	})
	if res.Recorded > 0 {
		fmt.Fprintf(out, "[town-metrics] recorded %d install regime events (%d already recorded)\n", res.Recorded, res.Skipped)
	}
	if err != nil {
		return fmt.Errorf("install regime events: %w", err)
	}
	return nil
}
