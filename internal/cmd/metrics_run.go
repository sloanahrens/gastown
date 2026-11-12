package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

func init() {
	metricsCmd.AddCommand(metricsRunCmd)
}

var metricsRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run one town-metrics hour: snapshot, install regime events, Jev dataset refresh, then analyse, under the single-flight lock",
	Long: `Takes <town>/.runtime/town-metrics.lock, then runs the snapshot, records the
install receipts the regime log does not hold, the Jev dataset refresh once in
each period the analysis is due, and the analysis, passing the receipts through.
A run that finds the lock held by another run prints a skipped line and exits 0,
so the daemon records the hour as skipped rather than as a failure. Every step
runs whichever one fails, and the command fails with the first failing step's
error. A dataset failure is logged one line each and does not fail the command.

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

// datasetStampName is the file beside the analysis stamp that names the due
// period the Jev datasets are current for.
const datasetStampName = "datasets.stamp"

// metricsHourSteps are the hour's jobs after the lock. Production passes the
// functions below; a test passes fakes, so the hour's order and its error
// precedence are driven without the snapshot's probes or the analysis's commands.
type metricsHourSteps struct {
	snapshot func(context.Context, io.Writer, string) error
	regime   func(io.Writer, string) error
	// analyse runs the daily analysis. beforeRun is called once, before the
	// analysis's first step, with the due period, on an hour the analysis runs
	// and not otherwise.
	analyse func(ctx context.Context, out io.Writer, town string, beforeRun func(context.Context, string)) error
	// dataset refreshes every enabled battery's Jev dataset and returns how many
	// it wrote and each failure.
	dataset func(ctx context.Context, town string) (int, []error)
}

// hostMetricsHourSteps are the hour's jobs over the town it runs in.
func hostMetricsHourSteps() metricsHourSteps {
	return metricsHourSteps{
		snapshot: runMetricsSnapshot,
		regime: func(out io.Writer, town string) error {
			return recordInstallRegimeEvents(out, metrics.OSEnv(town), townMetricsActor)
		},
		analyse: func(ctx context.Context, out io.Writer, town string, beforeRun func(context.Context, string)) error {
			opts := analysisOptions(town)
			opts.BeforeRun = beforeRun
			return runMetricsAnalyse(ctx, out, metrics.OSEnv(town), opts)
		},
		dataset: jevDatasetHour,
	}
}

// runMetricsHour is one hour of the plugin: the lock, then the steps in order.
// The snapshot's failure is the one reported when several fail, because the
// hour's record is the snapshot.
func runMetricsHour(ctx context.Context, out io.Writer, town string) error {
	return runMetricsHourWith(ctx, out, town, hostMetricsHourSteps())
}

// runMetricsHourWith is runMetricsHour over steps. The dataset refresh is the
// analysis's beforeRun, so it runs only when the analysis is due, decided by the
// analysis's own stamp check, and before the analysis's first step. It writes a
// stamp naming the due period, so the other hours of that period skip it while a
// failing analysis retries. A dataset failure is logged one line each and never
// stops the analysis or fails the hour.
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

	refresh := func(ctx context.Context, period string) {
		refreshDatasets(ctx, out, town, period, steps.dataset)
	}
	snapshotErr := steps.snapshot(ctx, out, town)
	regimeErr := steps.regime(out, town)
	analyseErr := steps.analyse(ctx, out, town, refresh)
	if snapshotErr != nil {
		return snapshotErr
	}
	if regimeErr != nil {
		return regimeErr
	}
	return analyseErr
}

// refreshDatasets refreshes the Jev datasets for period unless the stamp beside
// the analysis stamp names that same period, so a period whose analysis failed
// after the refresh does not export them again on its next hour. The stamp is
// written only when every battery and question exported, so a refresh that lost
// a pair is retried by the period's next hour. Nothing here fails the hour: a
// stamp that cannot be read or written is logged one line, and an unreadable
// stamp refreshes anyway rather than leaving the datasets stale on a guess.
func refreshDatasets(ctx context.Context, out io.Writer, town, period string, dataset func(context.Context, string) (int, []error)) {
	stampPath := filepath.Join(metrics.AnalysisDir(town), datasetStampName)
	current, err := datasetsCurrentFor(stampPath, period)
	if err != nil {
		fmt.Fprintf(out, "[town-metrics] jev dataset: %v; refreshing anyway\n", err)
	}
	if current {
		return
	}
	written, failures := dataset(ctx, town)
	fmt.Fprintf(out, "[town-metrics] jev dataset: %d written\n", written)
	for _, f := range failures {
		fmt.Fprintf(out, "[town-metrics] jev dataset: %v\n", f)
	}
	if len(failures) > 0 {
		return
	}
	if err := writeDatasetStamp(stampPath, period); err != nil {
		fmt.Fprintf(out, "[town-metrics] jev dataset: %v\n", err)
	}
}

// datasetsCurrentFor reports whether the dataset stamp names period. A missing
// stamp is not current; one that cannot be read is an error, which the hour
// logs before refreshing anyway.
func datasetsCurrentFor(stampPath, period string) (bool, error) {
	data, err := os.ReadFile(stampPath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the dataset stamp %s: %w", stampPath, err)
	}
	return strings.TrimSpace(string(data)) == datasetStampLine(period), nil
}

// writeDatasetStamp records that the datasets are current for period. A write
// need not be atomic: a stamp torn by a crash does not name the period, which
// reads as not current, so the next hour refreshes again.
func writeDatasetStamp(stampPath, period string) error {
	if err := os.MkdirAll(filepath.Dir(stampPath), 0o755); err != nil {
		return fmt.Errorf("jev dataset stamp %s: %w", stampPath, err)
	}
	if err := os.WriteFile(stampPath, []byte(datasetStampLine(period)+"\n"), 0o644); err != nil {
		return fmt.Errorf("jev dataset stamp %s: %w", stampPath, err)
	}
	return nil
}

// datasetStampLine is the dataset stamp's one line; the read and the write share
// it, so the two cannot disagree on the format.
func datasetStampLine(period string) string {
	return "datasets refreshed for due period " + period
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
