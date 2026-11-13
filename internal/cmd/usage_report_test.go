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

	"github.com/steveyegge/gastown/internal/dashboard"
)

var updateUsageReportGolden = flag.Bool("update-usage-report-golden", false, "rewrite the testdata/usage_report_*.golden files")

// reportNow is the report clock. The default one-day window is the day before it.
var reportNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// reportTown is a town whose usage records are the fixture of the usage report: two beads with Claude
// runs, one DeepSeek run, one incomplete run, one run with no end, and one run a day older than the
// window. Two unmatched sessions were last written in the window and one before it.
func reportTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	usageDir := filepath.Join(town, ".runtime", "usage")
	recs := []dashboard.UsageRecord{
		reportRecordFixture("gt-a", dashboard.UsageKindWork, dashboard.UsageClassPolecat, "2026-11-13T10:00:00Z", 1500, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 4, Input: 100, CacheCreation: 40, CacheRead: 900, Output: 25}}, 10),
		reportRecordFixture("gt-a", dashboard.UsageKindOM, dashboard.UsageClassOMReview, "2026-11-13T09:30:00Z", 300, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 2, Input: 50, Output: 10}}, 0),
		reportRecordFixture("gt-b", dashboard.UsageKindWork, dashboard.UsageClassPolecat, "2026-11-13T11:00:00Z", 600, true,
			map[string]dashboard.UsageModel{"deepseek-flash": {Calls: 3, Input: 2000, Output: 500, EstCostUSD: 0.0123}}, 500),
		reportRecordFixture("gt-c", dashboard.UsageKindWork, dashboard.UsageClassPolecat, "2026-11-13T08:00:00Z", 1800, false,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 9, Input: 999}}, 0),
		reportRecordFixture("gt-d", dashboard.UsageKindWork, dashboard.UsageClassDeepReview, "2026-11-12T13:00:00Z", 400, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 1, Input: 20, Output: 5}}, 0),
		reportRecordFixture("gt-old", dashboard.UsageKindWork, dashboard.UsageClassPolecat, "2026-11-10T00:00:00Z", 900, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 5, Input: 1000}}, 0),
		reportRecordFixture("gt-z", dashboard.UsageKindWork, dashboard.UsageClassPolecat, "", 900, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 7, Input: 7}}, 0),
	}
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		writeAttribFile(t, filepath.Join(usageDir, "beads", r.Bead+"."+r.Kind+".json"), string(b))
	}
	writeAttribFile(t, filepath.Join(usageDir, "unmatched.jsonl"),
		`{"seat":"gastown/nux","session":"s1","last_write":"2026-11-13T05:00:00Z","reason":"no run of its seat holds its last write"}`+"\n"+
			`{"seat":"gastown/nux","session":"s2","last_write":"2026-11-13T06:00:00Z","reason":"no run of its seat holds its last write"}`+"\n"+
			`{"seat":"gastown/nux","session":"s3","last_write":"2026-11-01T00:00:00Z","reason":"no run of its seat holds its last write"}`+"\n")
	return town
}

// reportRecordFixture is one usage record. An empty end is the zero time, a record with no end.
func reportRecordFixture(bead, kind, class, end string, wallS int64, complete bool,
	models map[string]dashboard.UsageModel, subUncached int64) dashboard.UsageRecord {
	rec := dashboard.UsageRecord{
		Bead: bead, Kind: kind, Class: class, Rig: "gastown", WallS: wallS, Complete: complete,
		Models:    models,
		Subagents: dashboard.UsageSubagents{Uncached: subUncached},
	}
	if end != "" {
		rec.End = mustReportTime(end)
	}
	return rec
}

func mustReportTime(s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return ts
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
	d := reportDepsFor(town, reportNow)
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
	d := reportDepsFor(town, reportNow)
	got, _, code := execUsageReport(t, d, "--by", "model", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	compareReportGolden(t, "usage_report_model.json.golden", got)
}

func TestUsageReportCountsAlwaysPrinted(t *testing.T) {
	t.Parallel()
	town := reportTown(t)
	d := reportDepsFor(town, reportNow)
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
	d := reportDepsFor(town, reportNow)

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
			d := reportDepsFor(town, reportNow)
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

// reportFailingWriter refuses every write, as a closed pipe does.
type reportFailingWriter struct{}

func (reportFailingWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

func TestUsageReportWriteFailureExitsTwo(t *testing.T) {
	t.Parallel()
	cmd := newUsageReportCmd(reportDepsFor(reportTown(t), reportNow))
	var errOut bytes.Buffer
	cmd.SetOut(reportFailingWriter{})
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
}
