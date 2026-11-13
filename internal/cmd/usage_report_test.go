package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/usagefixture"
)

var updateUsageReportGolden = flag.Bool("update-usage-report-golden", false, "rewrite the testdata/usage_report_*.golden files")

// reportTown is a town whose usage records are usagefixture, written as the scan leaves them on disk:
// one file per bead and kind, and unmatched.jsonl.
func reportTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	usageDir := filepath.Join(town, ".runtime", "usage")
	for _, r := range usagefixture.Records() {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		writeAttribFile(t, filepath.Join(usageDir, "beads", r.Bead+"."+r.Kind+".json"), string(b))
	}
	writeAttribFile(t, filepath.Join(usageDir, "unmatched.jsonl"), usagefixture.UnmatchedJSONL())
	return town
}

// reportDepsFor runs the report against town with the clock fixed at now. The zone is a fixed UTC-6
// standing in for America/Chicago's winter offset, so the test does not read the host's zone database.
func reportDepsFor(town string, now time.Time) usageReportDeps {
	return usageReportDeps{
		townRoot: func() (string, error) { return town, nil },
		now:      func() time.Time { return now },
		zone: func(name string) (*time.Location, error) {
			if name != attribZone {
				return nil, fmt.Errorf("unexpected zone %q", name)
			}
			return time.FixedZone("CST", -6*60*60), nil
		},
	}
}

// execUsageReport executes `gt usage report <args>` and returns its stdout, stderr, and the exit code the
// command reports (0 when it returns nil).
func execUsageReport(t *testing.T, d usageReportDeps, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newUsageReportCmd(d)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil {
		return out.String(), errOut.String(), 0
	}
	var exit *SilentExitError
	if errors.As(err, &exit) {
		return out.String(), errOut.String(), exit.Code
	}
	t.Fatalf("gt usage report %v: %v", args, err)
	return "", "", -1
}

// compareReportGolden checks got against testdata/<name>, rewriting it under -update-usage-report-golden.
func compareReportGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateUsageReportGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-usage-report-golden):\n%s\nwant:\n%s", path, got, want)
	}
}

func TestUsageReportGoldenTextByGrouping(t *testing.T) {
	t.Parallel()
	town := reportTown(t)
	d := reportDepsFor(town, usagefixture.Now())
	for _, by := range []string{"role", "model", "bead", "day"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			got, _, code := execUsageReport(t, d, "--by", by)
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			compareReportGolden(t, "usage_report_"+by+".golden", got)
		})
	}
}

func TestUsageReportGoldenJSONByModel(t *testing.T) {
	t.Parallel()
	town := reportTown(t)
	d := reportDepsFor(town, usagefixture.Now())
	got, _, code := execUsageReport(t, d, "--by", "model", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	compareReportGolden(t, "usage_report_model.json.golden", got)
}

func TestUsageReportCountsAlwaysPrinted(t *testing.T) {
	t.Parallel()
	town := reportTown(t)
	d := reportDepsFor(town, usagefixture.Now())
	// A bead with no record in the window still prints the three counts, with no rows.
	got, _, code := execUsageReport(t, d, "--bead", "gt-missing")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, line := range []string{"unmatched sessions: 2", "incomplete records: 1", "records with no end time: 1"} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("output lacks %q:\n%s", line, got)
		}
	}
}

func TestUsageReportBeadAndOverFlagsKeepTheirRows(t *testing.T) {
	t.Parallel()
	town := reportTown(t)
	d := reportDepsFor(town, usagefixture.Now())

	bead, _, code := execUsageReport(t, d, "--by", "bead", "--bead", "gt-b")
	if code != 0 || !strings.Contains(bead, "gt-b") || strings.Contains(bead, "gt-a") {
		t.Errorf("--bead gt-b: exit %d, output:\n%s\nwant only the gt-b row", code, bead)
	}

	over, _, code := execUsageReport(t, d, "--by", "bead", "--over", "20")
	if code != 0 || !strings.Contains(over, "gt-a") || strings.Contains(over, "gt-b") || strings.Contains(over, "gt-d") {
		t.Errorf("--over 20: exit %d, output:\n%s\nwant only the gt-a row (its work ran 25 minutes)", code, over)
	}
}

func TestUsageReportExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		setup      func(t *testing.T) string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{
			name:       "records directory missing",
			setup:      func(t *testing.T) string { return t.TempDir() },
			wantCode:   2,
			wantStderr: "records directory",
		},
		{
			name: "unmatched file missing, so the scan did not finish",
			setup: func(t *testing.T) string {
				town := reportTown(t)
				if err := os.Remove(filepath.Join(town, ".runtime", "usage", "unmatched.jsonl")); err != nil {
					t.Fatal(err)
				}
				return town
			},
			wantCode:   2,
			wantStderr: "unmatched.jsonl",
		},
		{
			name: "a record that does not decode",
			setup: func(t *testing.T) string {
				town := reportTown(t)
				writeAttribFile(t, filepath.Join(town, ".runtime", "usage", "beads", "gt-bad.work.json"), "{not a record")
				return town
			},
			wantCode:   2,
			wantStderr: "gt-bad.work.json",
		},
		{name: "days below one", setup: reportTown, args: []string{"--days", "0"}, wantCode: 2, wantStderr: "--days 0"},
		{name: "days past the duration range", setup: reportTown, args: []string{"--days", "999999999"}, wantCode: 2, wantStderr: "--days"},
		{name: "unknown grouping", setup: reportTown, args: []string{"--by", "week"}, wantCode: 2, wantStderr: "grouping \"week\""},
		{name: "negative over", setup: reportTown, args: []string{"--over", "-1"}, wantCode: 2, wantStderr: "--over -1"},
		{name: "a town that cannot be found", setup: func(t *testing.T) string { return "" }, wantCode: 2, wantStderr: "finding the town"},
		{name: "a valid report", setup: reportTown, wantCode: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := tt.setup(t)
			d := reportDepsFor(town, usagefixture.Now())
			if town == "" {
				d.townRoot = func() (string, error) { return "", errors.New("no town here") }
			}
			_, errOut, code := execUsageReport(t, d, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.wantCode, errOut)
			}
			if tt.wantStderr != "" && !strings.Contains(errOut, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to name %q", errOut, tt.wantStderr)
			}
		})
	}
}

// reportRefusingWriter accepts every write except one whose text holds refuse, as a pipe that closes
// partway through the report. The bytes it accepted are kept in buf.
type reportRefusingWriter struct {
	buf    bytes.Buffer
	refuse string
}

func (w *reportRefusingWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.refuse) {
		return 0, errors.New("closed pipe")
	}
	return w.buf.Write(p)
}

// TestUsageReportPartialWriteExitsTwo holds a pipe that closes at each part of the report, table and
// counts alike: a report that reached the reader only in part is never an exit 0.
func TestUsageReportPartialWriteExitsTwo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		refuse string
	}{
		{name: "the header", refuse: "usage by"},
		{name: "the table", refuse: "CALLS"},
		{name: "the unmatched count", refuse: "unmatched sessions"},
		{name: "the incomplete count", refuse: "incomplete records"},
		{name: "the skipped count", refuse: "records with no end time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := &reportRefusingWriter{refuse: tt.refuse}
			cmd := newUsageReportCmd(reportDepsFor(reportTown(t), usagefixture.Now()))
			var errOut bytes.Buffer
			cmd.SetOut(w)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{"--by", "role"})
			err := cmd.Execute()
			var exit *SilentExitError
			if !errors.As(err, &exit) || exit.Code != 2 {
				t.Fatalf("Execute error = %v, want exit 2", err)
			}
			if !strings.Contains(errOut.String(), "writing the report") {
				t.Errorf("stderr = %q, want it to name the failed write", errOut.String())
			}
			if strings.Contains(w.buf.String(), tt.refuse) {
				t.Errorf("output holds %q though the writer refused it", tt.refuse)
			}
		})
	}
}
