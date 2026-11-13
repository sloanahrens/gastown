package usageattrib

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// reportNow is the report clock. The default window is the day before it.
var reportNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// reportCST is the zone a day is read in, a fixed UTC-6 so the test does not read the host's zone database.
var reportCST = time.FixedZone("CST", -6*60*60)

func reportAt(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// reportRecord is one fixture record. subUncached is the subagents' uncached tokens.
func reportRecord(bead, kind, class string, end time.Time, wallS int64, complete bool,
	models map[string]dashboard.UsageModel, subUncached int64) dashboard.UsageRecord {
	return dashboard.UsageRecord{
		Bead: bead, Kind: kind, Class: class, Rig: "gastown", End: end, WallS: wallS, Complete: complete,
		Models:    models,
		Subagents: dashboard.UsageSubagents{Uncached: subUncached},
	}
}

func claudeUsage(calls, input, cacheCreation, output int64) dashboard.UsageModel {
	return dashboard.UsageModel{Calls: calls, Input: input, CacheCreation: cacheCreation, Output: output}
}

func deepseekUsage(calls, input, output int64, estUSD float64) dashboard.UsageModel {
	return dashboard.UsageModel{Calls: calls, Input: input, Output: output, EstCostUSD: estUSD}
}

// reportFixture is the town's records for a report at reportNow with the default one-day window:
//   - gt-a: a polecat work run and its om review, both in the window, 25 and 5 minutes long;
//   - gt-b: a polecat run on DeepSeek, in the window, 10 minutes long;
//   - gt-c: a polecat run still incomplete, in the window: counted as incomplete, left out of the rows;
//   - gt-d: a deep review ending the day before the newest day, in the window;
//   - gt-old: a run a day older than the window: counted nowhere;
//   - gt-z: a run with no end: counted as skipped.
//
// Two unmatched sessions were last written in the window and one before it.
func reportFixture(t *testing.T) ([]dashboard.UsageRecord, []time.Time) {
	t.Helper()
	recs := []dashboard.UsageRecord{
		reportRecord("gt-a", dashboard.UsageKindWork, dashboard.UsageClassPolecat, reportAt(t, "2026-11-13T10:00:00Z"), 1500, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": claudeUsage(4, 100, 40, 25)}, 10),
		reportRecord("gt-a", dashboard.UsageKindOM, dashboard.UsageClassOMReview, reportAt(t, "2026-11-13T09:30:00Z"), 300, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": claudeUsage(2, 50, 0, 10)}, 0),
		reportRecord("gt-b", dashboard.UsageKindWork, dashboard.UsageClassPolecat, reportAt(t, "2026-11-13T11:00:00Z"), 600, true,
			map[string]dashboard.UsageModel{"deepseek-flash": deepseekUsage(3, 2000, 500, 0.0123)}, 500),
		reportRecord("gt-c", dashboard.UsageKindWork, dashboard.UsageClassPolecat, reportAt(t, "2026-11-13T08:00:00Z"), 1800, false,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": claudeUsage(9, 999, 0, 0)}, 0),
		reportRecord("gt-d", dashboard.UsageKindWork, dashboard.UsageClassDeepReview, reportAt(t, "2026-11-12T13:00:00Z"), 400, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": claudeUsage(1, 20, 0, 5)}, 0),
		reportRecord("gt-old", dashboard.UsageKindWork, dashboard.UsageClassPolecat, reportAt(t, "2026-11-10T00:00:00Z"), 900, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": claudeUsage(5, 1000, 0, 0)}, 0),
		reportRecord("gt-z", dashboard.UsageKindWork, dashboard.UsageClassPolecat, time.Time{}, 900, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": claudeUsage(7, 7, 0, 0)}, 0),
	}
	unmatched := []time.Time{
		reportAt(t, "2026-11-13T05:00:00Z"),
		reportAt(t, "2026-11-13T06:00:00Z"),
		reportAt(t, "2026-11-01T00:00:00Z"),
	}
	return recs, unmatched
}

// wantRow is one expected row. sub is nil where the grouping has no subagent share.
type wantRow struct {
	key    string
	calls  int64
	claude int64
	ds     float64
	sub    *float64
}

func reportPtr(f float64) *float64 { return &f }

func reportNear(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// checkReport asserts the rows and the window counts of rep. The counts are the fixture's: two
// unmatched, one incomplete, one skipped, whatever filter the rows went through.
func checkReport(t *testing.T, rep Report, want []wantRow) {
	t.Helper()
	if rep.Unmatched != 2 || rep.Incomplete != 1 || rep.Skipped != 1 {
		t.Errorf("counts unmatched=%d incomplete=%d skipped=%d, want 2, 1, 1", rep.Unmatched, rep.Incomplete, rep.Skipped)
	}
	if len(rep.Rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rep.Rows), len(want), rep.Rows)
	}
	for i, w := range want {
		got := rep.Rows[i]
		if got.Key != w.key || got.Calls != w.calls || got.ClaudeUncached != w.claude || !reportNear(got.DeepSeekEstUSD, w.ds) {
			t.Errorf("row %d = %+v, want key %q calls %d claude %d deepseek %v", i, got, w.key, w.calls, w.claude, w.ds)
		}
		switch {
		case w.sub == nil && got.SubShare != nil:
			t.Errorf("row %q sub_share = %v, want none", w.key, *got.SubShare)
		case w.sub != nil && (got.SubShare == nil || !reportNear(*got.SubShare, *w.sub)):
			t.Errorf("row %q sub_share = %v, want %v", w.key, got.SubShare, *w.sub)
		}
	}
}

func TestUsageReportGroupsFixtureRecords(t *testing.T) {
	t.Parallel()
	recs, unmatched := reportFixture(t)
	from, to := reportNow.Add(-24*time.Hour), reportNow
	cst := reportCST
	tests := []struct {
		name string
		by   string
		zone *time.Location
		want []wantRow
	}{
		{
			name: "role",
			by:   ByRole,
			want: []wantRow{
				{key: dashboard.UsageClassDeepReview, calls: 1, claude: 25, sub: reportPtr(0)},
				{key: dashboard.UsageClassOMReview, calls: 2, claude: 60, sub: reportPtr(0)},
				{key: dashboard.UsageClassPolecat, calls: 7, claude: 165, ds: 0.0123, sub: reportPtr(510.0 / 2665.0)},
			},
		},
		{
			name: "model: Claude in tokens, DeepSeek in estimated cost, no subagent share",
			by:   ByModel,
			want: []wantRow{
				{key: "claude-sonnet-5-5", calls: 7, claude: 250},
				{key: "deepseek-flash", calls: 3, ds: 0.0123},
			},
		},
		{
			name: "bead",
			by:   ByBead,
			want: []wantRow{
				{key: "gt-a", calls: 6, claude: 225, sub: reportPtr(10.0 / 225.0)},
				{key: "gt-b", calls: 3, ds: 0.0123, sub: reportPtr(500.0 / 2500.0)},
				{key: "gt-d", calls: 1, claude: 25, sub: reportPtr(0)},
			},
		},
		{
			name: "day in the town zone",
			by:   ByDay,
			zone: cst,
			want: []wantRow{
				{key: "2026-11-12", calls: 1, claude: 25, sub: reportPtr(0)},
				{key: "2026-11-13", calls: 9, claude: 225, ds: 0.0123, sub: reportPtr(510.0 / 2725.0)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rep, err := BuildReport(recs, unmatched, ReportOptions{From: from, To: to, By: tt.by, Zone: tt.zone})
			if err != nil {
				t.Fatal(err)
			}
			checkReport(t, rep, tt.want)
		})
	}
}

func TestUsageReportBeadAndOverFilters(t *testing.T) {
	t.Parallel()
	recs, unmatched := reportFixture(t)
	from, to := reportNow.Add(-24*time.Hour), reportNow
	tests := []struct {
		name string
		opts ReportOptions
		want []wantRow
	}{
		{
			name: "bead keeps only that bead",
			opts: ReportOptions{By: ByRole, Bead: "gt-b"},
			want: []wantRow{{key: dashboard.UsageClassPolecat, calls: 3, ds: 0.0123, sub: reportPtr(500.0 / 2500.0)}},
		},
		{
			name: "over 20 minutes keeps the beads whose work ran longer, and every record of them",
			opts: ReportOptions{By: ByBead, Over: 20 * time.Minute},
			want: []wantRow{{key: "gt-a", calls: 6, claude: 225, sub: reportPtr(10.0 / 225.0)}},
		},
		{
			name: "over 20 minutes by role keeps the kept bead's om review as well",
			opts: ReportOptions{By: ByRole, Over: 20 * time.Minute},
			want: []wantRow{
				{key: dashboard.UsageClassOMReview, calls: 2, claude: 60, sub: reportPtr(0)},
				{key: dashboard.UsageClassPolecat, calls: 4, claude: 165, sub: reportPtr(10.0 / 165.0)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := tt.opts
			o.From, o.To = from, to
			rep, err := BuildReport(recs, unmatched, o)
			if err != nil {
				t.Fatal(err)
			}
			checkReport(t, rep, tt.want)
		})
	}
}

func TestUsageReportOverIsStrictlyOver(t *testing.T) {
	t.Parallel()
	at := reportAt(t, "2026-11-13T11:00:00Z")
	var recs []dashboard.UsageRecord
	for _, b := range []struct {
		bead  string
		wallS int64
	}{{"gt-under", 1199}, {"gt-at", 1200}, {"gt-over", 1201}} {
		recs = append(recs, reportRecord(b.bead, dashboard.UsageKindWork, dashboard.UsageClassPolecat, at, b.wallS, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": claudeUsage(1, 1, 0, 0)}, 0))
	}
	rep, err := BuildReport(recs, nil, ReportOptions{
		From: reportNow.Add(-24 * time.Hour), To: reportNow, By: ByBead, Over: 20 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].Key != "gt-over" {
		t.Errorf("rows = %+v, want only gt-over: a bead exactly at the limit is not over it", rep.Rows)
	}
}

func TestUsageReportWindowIncludesBothEnds(t *testing.T) {
	t.Parallel()
	from, to := reportNow.Add(-24*time.Hour), reportNow
	models := map[string]dashboard.UsageModel{"claude-sonnet-5-5": claudeUsage(1, 1, 0, 0)}
	recs := []dashboard.UsageRecord{
		reportRecord("gt-from", dashboard.UsageKindWork, dashboard.UsageClassPolecat, from, 60, true, models, 0),
		reportRecord("gt-to", dashboard.UsageKindWork, dashboard.UsageClassPolecat, to, 60, true, models, 0),
		reportRecord("gt-before", dashboard.UsageKindWork, dashboard.UsageClassPolecat, from.Add(-time.Nanosecond), 60, true, models, 0),
		reportRecord("gt-after", dashboard.UsageKindWork, dashboard.UsageClassPolecat, to.Add(time.Nanosecond), 60, true, models, 0),
	}
	unmatched := []time.Time{from, from.Add(-time.Nanosecond)}
	rep, err := BuildReport(recs, unmatched, ReportOptions{From: from, To: to, By: ByBead})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range rep.Rows {
		keys = append(keys, r.Key)
	}
	if strings.Join(keys, ",") != "gt-from,gt-to" {
		t.Errorf("rows = %v, want gt-from and gt-to: a record ending exactly on either bound is in the window", keys)
	}
	if rep.Unmatched != 1 {
		t.Errorf("unmatched = %d, want 1: a session written at the start bound counts, one a nanosecond before does not", rep.Unmatched)
	}
}

func TestUsageReportRefusesUnknownGrouping(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"", "week", "Role"} {
		if _, err := BuildReport(nil, nil, ReportOptions{By: by}); err == nil {
			t.Errorf("grouping %q: no error, want a refusal", by)
		}
	}
}

func reportWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUsageReportReadRecordsDecodesEveryRecordFile(t *testing.T) {
	t.Parallel()
	usageDir := t.TempDir()
	recs, _ := reportFixture(t)
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		reportWriteFile(t, recordPath(usageDir, r.Bead, r.Kind), string(b))
	}
	got, err := ReadRecords(usageDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(recs) {
		t.Errorf("read %d records, want %d", len(got), len(recs))
	}
}

func TestUsageReportReadRecordsRefusesMalformedFile(t *testing.T) {
	t.Parallel()
	usageDir := t.TempDir()
	reportWriteFile(t, recordPath(usageDir, "gt-bad", dashboard.UsageKindWork), "{not a record")
	_, err := ReadRecords(usageDir)
	if err == nil || !strings.Contains(err.Error(), "gt-bad.work.json") {
		t.Errorf("ReadRecords error = %v, want a refusal naming the file", err)
	}
}

func TestUsageReportReadRecordsMissingDirectoryIsNotExist(t *testing.T) {
	t.Parallel()
	_, err := ReadRecords(t.TempDir())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadRecords error = %v, want one wrapping fs.ErrNotExist", err)
	}
}

func TestUsageReportReadUnmatchedRefusesMissingAndMalformedFiles(t *testing.T) {
	t.Parallel()
	missing := t.TempDir()
	if _, err := ReadUnmatched(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing unmatched.jsonl: error = %v, want one wrapping fs.ErrNotExist", err)
	}
	bad := t.TempDir()
	reportWriteFile(t, filepath.Join(bad, UnmatchedFile), "{\"seat\":\"gastown/nux\"}\nnot json\n")
	if _, err := ReadUnmatched(bad); err == nil {
		t.Error("malformed unmatched.jsonl: no error, want a refusal")
	}
}

func TestUsageReportReadUnmatchedCountsEachLine(t *testing.T) {
	t.Parallel()
	usageDir := t.TempDir()
	reportWriteFile(t, filepath.Join(usageDir, UnmatchedFile),
		"{\"seat\":\"gastown/nux\",\"session\":\"s1\",\"last_write\":\"2026-11-13T05:00:00Z\",\"reason\":\"r\"}\n\n"+
			"{\"seat\":\"gastown/nux\",\"session\":\"s2\",\"last_write\":\"2026-11-13T06:00:00Z\",\"reason\":\"r\"}\n")
	writes, err := ReadUnmatched(usageDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 2 {
		t.Errorf("read %d unmatched sessions, want 2 (a blank line is not one)", len(writes))
	}
}

func TestUsageReportReadRecordsRefusesUnreadableFile(t *testing.T) {
	t.Parallel()
	usageDir := t.TempDir()
	dir := filepath.Join(usageDir, recordsSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A dangling link is listed as a record file, but its content cannot be read.
	if err := os.Symlink(filepath.Join(usageDir, "missing"), filepath.Join(dir, "gt-dangling.work.json")); err != nil {
		t.Fatal(err)
	}
	_, err := ReadRecords(usageDir)
	if err == nil || !strings.Contains(err.Error(), "gt-dangling.work.json") {
		t.Errorf("ReadRecords error = %v, want a refusal naming the unreadable file", err)
	}
}
