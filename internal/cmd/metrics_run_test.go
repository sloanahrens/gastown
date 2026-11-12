package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
				analyse: func(context.Context, io.Writer, string) error {
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
