package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/metrics"
)

// A run that finds the lock held by another run records the hour as skipped and
// writes nothing: no snapshot is taken and no analysis runs. The lock is held
// here by a second open file description in this process, which is what a
// second run is to flock.
func TestMetricsRunSkipsAnHourWhenAnotherRunHoldsTheLock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	lockPath := filepath.Join(town, ".runtime", "town-metrics.lock")
	held, err := metrics.AcquireTownLock(lockPath)
	if err != nil {
		t.Fatalf("hold the lock: %v", err)
	}
	defer held.Release()

	var out bytes.Buffer
	if err := runMetricsHour(context.Background(), &out, town); err != nil {
		t.Fatalf("runMetricsHour with the lock held: %v", err)
	}
	got := out.String()
	if !strings.HasSuffix(got, "[plugin-result skipped]\n") {
		t.Errorf("output = %q, want it to end with the skipped marker", got)
	}
	if _, err := os.Stat(metrics.SnapshotPath(town)); !os.IsNotExist(err) {
		t.Errorf("a snapshot was appended while the lock was held (stat err = %v)", err)
	}
}

// The hour runs the snapshot, the regime step and the analysis in that order,
// every step runs even when an earlier one fails, and the snapshot's error is
// the one reported when several fail.
func TestMetricsHourRunsEveryStepAndReportsTheFirstFailure(t *testing.T) {
	t.Parallel()
	errSnapshot := errors.New("snapshot failed")
	errRegime := errors.New("regime failed")
	errAnalyse := errors.New("analyse failed")
	tests := []struct {
		name                               string
		snapshotErr, regimeErr, analyseErr error
		want                               error
	}{
		{name: "all succeed", want: nil},
		{name: "the snapshot fails", snapshotErr: errSnapshot, want: errSnapshot},
		{name: "the regime step fails", regimeErr: errRegime, want: errRegime},
		{name: "the analysis fails", analyseErr: errAnalyse, want: errAnalyse},
		{name: "the snapshot and the regime step fail", snapshotErr: errSnapshot, regimeErr: errRegime, want: errSnapshot},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			steps := metricsHourSteps{
				snapshot: func(context.Context, io.Writer, string) error {
					calls = append(calls, "snapshot")
					return tc.snapshotErr
				},
				regime: func(io.Writer, string) error {
					calls = append(calls, "regime")
					return tc.regimeErr
				},
				analyse: func(context.Context, io.Writer, string, func(context.Context, string)) error {
					calls = append(calls, "analyse")
					return tc.analyseErr
				},
			}
			err := runMetricsHourWith(context.Background(), io.Discard, t.TempDir(), steps)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := strings.Join(calls, ","); got != "snapshot,regime,analyse" {
				t.Fatalf("steps ran %s, want every step in order", got)
			}
		})
	}
}

// writeHourFile writes content at the town-relative path under town.
func writeHourFile(t *testing.T, town, rel, content string) {
	t.Helper()
	path := filepath.Join(town, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The hour records the install receipts into the regime log under the process
// actor, and a second step over the same receipts prints nothing because the
// log holds every event.
func TestRecordInstallRegimeEventsRecordsTheReceipts(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeHourFile(t, town, "daemon/install-receipts.jsonl",
		`{"commit":"abc123","event":"installed","ts":"2026-11-10T12:00:00Z"}`+"\n")

	var out bytes.Buffer
	if err := recordInstallRegimeEvents(&out, metrics.OSEnv(town), townMetricsActor); err != nil {
		t.Fatalf("recordInstallRegimeEvents: %v", err)
	}
	if !strings.Contains(out.String(), "recorded 1 install regime events") {
		t.Fatalf("output = %q, want the receipt counted", out.String())
	}
	data, err := os.ReadFile(metrics.RegimeEventsPath(town))
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if !strings.Contains(string(data), `"actor":"town-metrics"`) || !strings.Contains(string(data), `"after_ref":"abc123"`) {
		t.Fatalf("record = %q, want the install event under the process actor", data)
	}

	out.Reset()
	if err := recordInstallRegimeEvents(&out, metrics.OSEnv(town), townMetricsActor); err != nil {
		t.Fatalf("second recordInstallRegimeEvents: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("second step printed %q, want nothing recorded", out.String())
	}
}

// The hour's regime step fails with the wiring error, so the hour is escalated
// rather than reading a town it could not record as clean.
func TestRecordInstallRegimeEventsReturnsTheWiringError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := recordInstallRegimeEvents(&out, metrics.Env{Town: t.TempDir()}, townMetricsActor)
	if err == nil || !strings.Contains(err.Error(), "install regime events") || !strings.Contains(err.Error(), "ReadFile") {
		t.Fatalf("err = %v, want the wiring error under the step's name", err)
	}
}

// A receipts file the hour cannot read fails the step, so the hour exits non-zero,
// and the other receipts still record first.
func TestRecordInstallRegimeEventsReturnsAnUnreadableFileAndRecordsTheRest(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory where the installer's receipts file would be is a read the
	// host refuses, which is what a broken copy of the file looks like.
	if err := os.MkdirAll(filepath.Join(town, "daemon", "install-receipts.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHourFile(t, town, "daemon/install-om-receipts.jsonl",
		`{"commit":"om987","event":"installed","ts":"2026-11-11T01:30:00Z"}`+"\n")

	var out bytes.Buffer
	err := recordInstallRegimeEvents(&out, metrics.OSEnv(town), townMetricsActor)
	if err == nil || !strings.Contains(err.Error(), "install regime events") || !strings.Contains(err.Error(), "install-receipts.jsonl") {
		t.Fatalf("err = %v, want the unreadable receipts file named under the step's name", err)
	}
	if !strings.Contains(out.String(), "recorded 1 install regime events") {
		t.Fatalf("output = %q, want the readable receipt counted", out.String())
	}
	data, readErr := os.ReadFile(metrics.RegimeEventsPath(town))
	if readErr != nil {
		t.Fatalf("reading the record: %v", readErr)
	}
	if !strings.Contains(string(data), `"after_ref":"om987"`) {
		t.Fatalf("record = %q, want the readable receipt recorded", data)
	}
}

// analysis fake calls its BeforeRun only when due, as the real analysis does
// after its stamp check, and the dataset fake stands in for the real refresh.
func fakeHourSteps(calls *[]string, due bool, failures []error) metricsHourSteps {
	return metricsHourSteps{
		snapshot: func(context.Context, io.Writer, string) error {
			*calls = append(*calls, "snapshot")
			return nil
		},
		regime: func(io.Writer, string) error {
			*calls = append(*calls, "regime")
			return nil
		},
		dataset: func(context.Context, string) (int, []error) {
			*calls = append(*calls, "dataset")
			return 2, failures
		},
		analyse: func(ctx context.Context, _ io.Writer, _ string, beforeRun func(context.Context, string)) error {
			if due {
				beforeRun(ctx, "2026-11-12T00:00:00Z")
			}
			*calls = append(*calls, "analyse")
			return nil
		},
	}
}

// On a due hour the dataset is refreshed after the snapshot and before the
// analysis, under the same lock.
func TestMetricsHourRefreshesDatasetBeforeAnalysisWhenDue(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var calls []string
	var out bytes.Buffer
	if err := runMetricsHourWith(context.Background(), &out, town, fakeHourSteps(&calls, true, nil)); err != nil {
		t.Fatalf("runMetricsHourWith: %v", err)
	}
	want := []string{"snapshot", "regime", "dataset", "analyse"}
	if !slices.Equal(calls, want) {
		t.Errorf("steps ran %v, want %v", calls, want)
	}
	if !strings.Contains(out.String(), "[town-metrics] jev dataset: 2 written\n") {
		t.Errorf("output %q does not print the dataset receipt", out.String())
	}
}

// An hour the analysis is not due on does not refresh the datasets. The
// analysis itself still runs, and its own stamp check makes that hour a no-op.
func TestMetricsHourSkipsDatasetOnANotDueHour(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var calls []string
	var out bytes.Buffer
	if err := runMetricsHourWith(context.Background(), &out, town, fakeHourSteps(&calls, false, nil)); err != nil {
		t.Fatalf("runMetricsHourWith: %v", err)
	}
	want := []string{"snapshot", "regime", "analyse"}
	if !slices.Equal(calls, want) {
		t.Errorf("steps ran %v, want %v", calls, want)
	}
	if strings.Contains(out.String(), "jev dataset") {
		t.Errorf("output %q mentions the dataset on a not-due hour", out.String())
	}
}

// A dataset failure is logged on one line, and the analysis still runs and the
// hour still succeeds.
func TestMetricsHourLogsADatasetFailureOnceAndAnalysesStill(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var calls []string
	var out bytes.Buffer
	failure := errors.New("logtriage.needs_action: battery \"logtriage\" has no labeled rows")
	if err := runMetricsHourWith(context.Background(), &out, town, fakeHourSteps(&calls, true, []error{failure})); err != nil {
		t.Fatalf("a dataset failure failed the hour: %v", err)
	}
	if !slices.Contains(calls, "analyse") {
		t.Errorf("steps ran %v, want the analysis to run after the dataset failure", calls)
	}
	var lines []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.Contains(line, "has no labeled rows") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "[town-metrics] jev dataset: ") {
		t.Errorf("failure lines = %q, want exactly one dataset line", lines)
	}
}

// A refresh that had a failure leaves no stamp, so the period's next hour
// exports again rather than leaving the failed pairs stale until the next period.
func TestMetricsHourRetriesTheDatasetRefreshAfterAFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var calls []string
	failure := errors.New("logtriage.needs_action: battery \"logtriage\" has no labeled rows")
	steps := fakeHourSteps(&calls, true, []error{failure})
	for hour := 1; hour <= 2; hour++ {
		var out bytes.Buffer
		if err := runMetricsHourWith(context.Background(), &out, town, steps); err != nil {
			t.Fatalf("hour %d: %v", hour, err)
		}
		if got := strings.Count(strings.Join(calls, " "), "dataset"); got != hour {
			t.Fatalf("after hour %d the datasets exported %d times, want %d", hour, got, hour)
		}
	}
}

// An unreadable dataset stamp is logged one line and the refresh runs anyway: a
// stamp nobody can read must not leave the datasets stale.
func TestMetricsHourRefreshesDatasetsOnAnUnreadableStamp(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory where the stamp file goes makes every read of it fail.
	if err := os.MkdirAll(filepath.Join(metrics.AnalysisDir(town), datasetStampName), 0o755); err != nil {
		t.Fatalf("lay out an unreadable stamp: %v", err)
	}
	var calls []string
	var out bytes.Buffer
	if err := runMetricsHourWith(context.Background(), &out, town, fakeHourSteps(&calls, true, nil)); err != nil {
		t.Fatalf("runMetricsHourWith: %v", err)
	}
	if !slices.Contains(calls, "dataset") {
		t.Errorf("steps ran %v, want the refresh despite the unreadable stamp", calls)
	}
	if !strings.Contains(out.String(), "reading the dataset stamp") {
		t.Errorf("output %q does not name the unreadable stamp", out.String())
	}
}

// A dataset stamp that cannot be written is logged and fails nothing else: the
// refresh already ran, and the hour's analysis still runs.
func TestMetricsHourLogsADatasetStampItCannotWrite(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A file where the analysis directory goes makes the stamp unreadable and
	// unwritable.
	if err := os.MkdirAll(filepath.Join(town, ".runtime", "metrics"), 0o755); err != nil {
		t.Fatalf("lay out the town runtime: %v", err)
	}
	if err := os.WriteFile(metrics.AnalysisDir(town), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("lay out an unwritable stamp directory: %v", err)
	}
	var calls []string
	var out bytes.Buffer
	if err := runMetricsHourWith(context.Background(), &out, town, fakeHourSteps(&calls, true, nil)); err != nil {
		t.Fatalf("runMetricsHourWith: %v", err)
	}
	if !slices.Contains(calls, "analyse") {
		t.Errorf("steps ran %v, want the analysis after the stamp write failed", calls)
	}
	if !strings.Contains(out.String(), "jev dataset stamp") {
		t.Errorf("output %q does not name the stamp write that failed", out.String())
	}
}

// The refresh runs once a due period, decided by the analysis's own stamp check:
// the analysis is due and fails after the refresh every hour, so the period's
// next hour still finds it due and skips the refresh, and the next period
// exports again. Nothing here stubs the due decision: RunAnalysis reads the
// analysis stamp and beforeRun fires through the real BeforeRun seam.
func TestMetricsHourRefreshesDatasetOnceADuePeriod(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	exports := 0
	steps := metricsHourSteps{
		snapshot: func(context.Context, io.Writer, string) error { return nil },
		regime:   func(io.Writer, string) error { return nil },
		dataset: func(context.Context, string) (int, []error) {
			exports++
			return 1, nil
		},
		analyse: func(ctx context.Context, out io.Writer, _ string, beforeRun func(context.Context, string)) error {
			env := metrics.Env{
				Town:     town,
				Now:      func() time.Time { return now },
				ReadFile: os.ReadFile,
				Run: func(context.Context, []string) ([]byte, error) {
					return nil, errors.New("the analysis must fail before it runs a command")
				},
				RunEnv: func(context.Context, []string, []string) ([]byte, error) {
					return nil, errors.New("the analysis must fail before it runs a command")
				},
			}
			opts := analysisOptions(town)
			// No toolchain and no mango binary: the analysis fails after the
			// refresh, so it never writes its stamp and is due again next hour.
			opts.LookPath = func(string) (string, error) { return "", errors.New("no go toolchain in this test") }
			opts.Extract = func(context.Context, string, string) (string, error) {
				return "", errors.New("the analysis must fail before the extract")
			}
			opts.BeforeRun = beforeRun
			return runMetricsAnalyse(ctx, out, env, opts)
		},
	}
	hours := []struct {
		advance time.Duration
		want    int
		why     string
	}{
		{0, 1, "the due hour refreshes the datasets"},
		{time.Hour, 1, "the period's next hour does not refresh again"},
		{25 * time.Hour, 2, "the next period refreshes again"},
	}
	for i, h := range hours {
		now = now.Add(h.advance)
		var out bytes.Buffer
		if err := runMetricsHourWith(context.Background(), &out, town, steps); err == nil {
			t.Fatalf("%s: the hour succeeded though the analysis fails", h.why)
		}
		if exports != h.want {
			t.Fatalf("%s: datasets exported %d times, want %d", h.why, exports, h.want)
		}
		if i == 0 {
			stamp, err := os.ReadFile(filepath.Join(metrics.AnalysisDir(town), datasetStampName))
			if err != nil {
				t.Fatalf("the refresh wrote no dataset stamp: %v", err)
			}
			if period := metrics.DuePeriod(time.Time{}, now, metrics.DailyEvery); !strings.Contains(string(stamp), period) {
				t.Fatalf("dataset stamp %q does not name the due period %s", stamp, period)
			}
		}
	}
}
