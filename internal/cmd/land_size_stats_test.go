package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
)

// landSizeStatsNow is the instant every case runs at: a Friday in the week
// starting 2026-11-09, so a four-week window is 10-19 to 11-09.
var landSizeStatsNow = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

// landSizeStatsWant is one bucket row as a case states it: the figures the
// table prints, so a case compares numbers rather than column spacing.
type landSizeStatsWant struct {
	label  string
	beads  string
	median string
	multi  string
	over30 string
}

// landSizeStatsLabels are the six buckets in report order.
var landSizeStatsLabels = []string{"under 50", "50 to 149", "150 to 299", "300 to 599", "600 to 999", "1000 or more"}

// landSizeStatsFixture writes a town whose landings cover all six buckets: the
// records SizeStats leaves out, a landing outside the window, a record with no
// landed_at, and a line that is not a record. The records sit in four weeks and
// two rigs, so a case can show the window cutting by week and the read
// covering every ledger.
func landSizeStatsFixture(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	dir := landReworkStatsTownDir(t, town)

	at := func(week time.Time, i int) time.Time {
		return week.Add(time.Duration(i) * time.Hour)
	}
	rec := func(bead string, added int, secs float64, seats int, landed time.Time) landings.Record {
		r := landings.Record{Bead: bead, Rig: "gastown", AddedLines: added, PolecatSecs: secs, LandedAt: landed}
		for i := 0; i < seats; i++ {
			r.PolecatAttempts = append(r.PolecatAttempts, land.PolecatAttempt{Seat: fmt.Sprintf("%s-seat-%d", bead, i)})
		}
		return r
	}
	w1 := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	w2 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	w3 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	w4 := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

	gastown := []landings.Record{
		// under 50: 10, 20 and 40 minutes; the 20-minute one took two seats, the
		// 40-minute one recorded no seat, so it counts as a single attempt.
		rec("gt-a1", 10, 600, 1, at(w1, 0)),
		rec("gt-a2", 49, 1200, 2, at(w2, 0)),
		rec("gt-a3", 25, 2400, 0, at(w3, 0)),
		// 50 to 149: 5 and 7 minutes.
		rec("gt-b1", 50, 300, 1, at(w1, 1)),
		rec("gt-b2", 149, 420, 1, at(w3, 1)),
		// 150 to 299: exactly 30 minutes is not over 30; 50 minutes is.
		rec("gt-c1", 150, 1800, 1, at(w3, 2)),
		rec("gt-c2", 299, 3000, 2, at(w1, 2)),
		// 300 to 599: 60 minutes.
		rec("gt-d1", 300, 3600, 1, at(w2, 1)),
		// 600 to 999: 15 minutes on two seats.
		rec("gt-e1", 600, 900, 2, at(w2, 2)),
		// 1000 or more: 90 minutes and three seats.
		rec("gt-f1", 1000, 5400, 3, at(w3, 3)),
		// The records no bucket can take, and the ones the window cannot place.
		rec("gt-notime", 100, 0, 1, at(w4, 0)),
		rec("gt-nolines", 0, 600, 1, at(w4, 1)),
		{
			Bead: "gt-bound", Rig: "gastown", AddedLines: 30, PolecatSecs: 3600,
			PolecatUpperBound: true, LandedAt: at(w4, 2),
		},
		rec("gt-old", 100, 600, 1, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)),
		{Bead: "gt-nodate", Rig: "gastown", AddedLines: 100, PolecatSecs: 600},
	}
	longeye := []landings.Record{
		rec("gt-d2", 599, 60, 1, at(w4, 3)),
		rec("gt-e2", 999, 1200, 1, at(w4, 4)),
	}
	writeLandReworkStatsLedger(t, filepath.Join(dir, "gastown.jsonl"), gastown, "this line is not a landing record")
	writeLandReworkStatsLedger(t, filepath.Join(dir, "longeye.jsonl"), longeye)
	return town
}

// landSizeStatsTable runs the table form and returns it.
func landSizeStatsTable(t *testing.T, town string, weeks int) string {
	t.Helper()
	var out bytes.Buffer
	if err := landSizeStatsReportFrom(landReworkStatsTown(town), &out, landSizeStatsNow, weeks, false); err != nil {
		t.Fatalf("the report failed: %v", err)
	}
	return out.String()
}

// landSizeStatsReportOf runs the JSON form and decodes it.
func landSizeStatsReportOf(t *testing.T, town string, weeks int) landSizeStatsReport {
	t.Helper()
	var out bytes.Buffer
	if err := landSizeStatsReportFrom(landReworkStatsTown(town), &out, landSizeStatsNow, weeks, true); err != nil {
		t.Fatalf("the report failed: %v", err)
	}
	var report landSizeStatsReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decoding the report: %v\n%s", err, out.String())
	}
	return report
}

// landSizeStatsRow returns the table's cells for one bucket, in the order the
// header names them.
func landSizeStatsRow(t *testing.T, table, label string) []string {
	t.Helper()
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, label+" ") {
			continue
		}
		cells := strings.Fields(strings.TrimPrefix(line, label))
		if len(cells) != 4 {
			t.Fatalf("row for %q has %d cells, want 4: %q", label, len(cells), line)
		}
		return cells
	}
	t.Fatalf("the table has no row for %q:\n%s", label, table)
	return nil
}

// checkLandSizeStatsRows compares every bucket row the table printed with the
// figures a case states.
func checkLandSizeStatsRows(t *testing.T, table string, want []landSizeStatsWant) {
	t.Helper()
	last := -1
	for _, w := range want {
		cells := landSizeStatsRow(t, table, w.label)
		got := []string{cells[0], cells[1], cells[2], cells[3]}
		expect := []string{w.beads, w.median, w.multi, w.over30}
		for i := range expect {
			if got[i] != expect[i] {
				t.Errorf("%s: cell %d = %q, want %q (row %v)", w.label, i, got[i], expect[i], got)
			}
		}
		if pos := strings.Index(table, w.label); pos < last {
			t.Errorf("%s: bucket rows are out of report order:\n%s", w.label, table)
		} else {
			last = pos
		}
	}
}

// TestRunawayBeadSizeStatsTable: the table prints every bucket's beads, median
// minutes and both shares, and the counts the rows leave out, so a short table
// is distinguishable from a complete one.
func TestRunawayBeadSizeStatsTable(t *testing.T) {
	t.Parallel()
	table := landSizeStatsTable(t, landSizeStatsFixture(t), 4)
	if !strings.Contains(table, "size stats: 2026-10-19 to 2026-11-09") {
		t.Errorf("the table does not name the window:\n%s", table)
	}
	checkLandSizeStatsRows(t, table, []landSizeStatsWant{
		{"under 50", "3", "20.0", "33.3%", "33.3%"},
		{"50 to 149", "2", "6.0", "0.0%", "0.0%"},
		{"150 to 299", "2", "40.0", "50.0%", "50.0%"},
		{"300 to 599", "2", "30.5", "0.0%", "50.0%"},
		{"600 to 999", "2", "17.5", "50.0%", "0.0%"},
		{"1000 or more", "1", "90.0", "100.0%", "100.0%"},
	})
	for _, note := range []string{
		"3 record(s) left out of every bucket",
		"1 landing(s) outside the window",
		"1 record(s) with no landed_at",
		"1 unreadable record line(s)",
	} {
		if !strings.Contains(table, note) {
			t.Errorf("the table does not print %q:\n%s", note, table)
		}
	}
}

// TestRunawayBeadSizeStatsWeeksLeaveOutOlder: --weeks narrows the window, and a
// landing in an older week is left out of every bucket and counted, not folded
// in. The two-week window holds one record per bucket, one of them from the
// second rig's ledger.
func TestRunawayBeadSizeStatsWeeksLeaveOutOlder(t *testing.T) {
	t.Parallel()
	table := landSizeStatsTable(t, landSizeStatsFixture(t), 2)
	if !strings.Contains(table, "size stats: 2026-11-02 to 2026-11-09") {
		t.Errorf("the table does not name the two-week window:\n%s", table)
	}
	checkLandSizeStatsRows(t, table, []landSizeStatsWant{
		{"under 50", "1", "40.0", "0.0%", "100.0%"},
		{"50 to 149", "1", "7.0", "0.0%", "0.0%"},
		{"150 to 299", "1", "30.0", "0.0%", "0.0%"},
		{"300 to 599", "1", "1.0", "0.0%", "0.0%"},
		{"600 to 999", "1", "20.0", "0.0%", "0.0%"},
		{"1000 or more", "1", "90.0", "100.0%", "100.0%"},
	})
	if !strings.Contains(table, "7 landing(s) outside the window") {
		t.Errorf("the table does not count the landings the window left out:\n%s", table)
	}
}

// TestRunawayBeadSizeStatsJSONMatchesTable: --json prints the same numbers the
// table renders, and the same counts beneath them.
func TestRunawayBeadSizeStatsJSONMatchesTable(t *testing.T) {
	t.Parallel()
	town := landSizeStatsFixture(t)
	want := []landSizeStatsWant{
		{"under 50", "3", "20.0", "33.3%", "33.3%"},
		{"50 to 149", "2", "6.0", "0.0%", "0.0%"},
		{"150 to 299", "2", "40.0", "50.0%", "50.0%"},
		{"300 to 599", "2", "30.5", "0.0%", "50.0%"},
		{"600 to 999", "2", "17.5", "50.0%", "0.0%"},
		{"1000 or more", "1", "90.0", "100.0%", "100.0%"},
	}
	table := landSizeStatsTable(t, town, 4)
	report := landSizeStatsReportOf(t, town, 4)

	if report.FirstWeek != "2026-10-19" || report.LastWeek != "2026-11-09" {
		t.Errorf("window = %s to %s, want 2026-10-19 to 2026-11-09", report.FirstWeek, report.LastWeek)
	}
	if len(report.Buckets) != len(want) {
		t.Fatalf("buckets = %d, want %d", len(report.Buckets), len(want))
	}
	for i, w := range want {
		row := report.Buckets[i]
		if row.Label != w.label {
			t.Errorf("bucket %d label = %q, want %q", i, row.Label, w.label)
		}
		if row.MedianMinutes == nil || row.MultiAttemptShare == nil || row.Over30Share == nil {
			t.Fatalf("%s: an empty figure in a bucket with records: %+v", w.label, row)
		}
		cells := landSizeStatsRow(t, table, w.label)
		got := []string{
			fmt.Sprint(row.Beads),
			fmt.Sprintf("%.1f", *row.MedianMinutes),
			fmt.Sprintf("%.1f%%", *row.MultiAttemptShare*100),
			fmt.Sprintf("%.1f%%", *row.Over30Share*100),
		}
		expect := []string{w.beads, w.median, w.multi, w.over30}
		for j := range expect {
			if got[j] != expect[j] || cells[j] != expect[j] {
				t.Errorf("%s: cell %d = table %q, json %q, want %q", w.label, j, cells[j], got[j], expect[j])
			}
		}
	}
	if report.Skipped != 3 || report.Outside != 1 || report.NoLandingTime != 1 || report.Unreadable != 1 {
		t.Errorf("skipped/outside/no_landing_time/unreadable = %d/%d/%d/%d, want 3/1/1/1",
			report.Skipped, report.Outside, report.NoLandingTime, report.Unreadable)
	}
}

// TestRunawayBeadSizeStatsEmptyTown: a town that has not landed yet prints the
// six buckets with zero beads and no figure at all, in both forms, rather than
// failing or printing zero minutes.
func TestRunawayBeadSizeStatsEmptyTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	table := landSizeStatsTable(t, town, 2)
	if !strings.Contains(table, "size stats: 2026-11-02 to 2026-11-09") {
		t.Errorf("the table does not name the window:\n%s", table)
	}
	for _, label := range landSizeStatsLabels {
		cells := landSizeStatsRow(t, table, label)
		want := []string{"0", "-", "-", "-"}
		for i := range want {
			if cells[i] != want[i] {
				t.Errorf("%s: cell %d = %q, want %q", label, i, cells[i], want[i])
			}
		}
	}

	report := landSizeStatsReportOf(t, town, 2)
	if len(report.Buckets) != len(landSizeStatsLabels) {
		t.Fatalf("buckets = %d, want %d", len(report.Buckets), len(landSizeStatsLabels))
	}
	for i, label := range landSizeStatsLabels {
		row := report.Buckets[i]
		if row.Label != label || row.Beads != 0 {
			t.Errorf("bucket %d = %q with %d beads, want empty %q", i, row.Label, row.Beads, label)
		}
		if row.MedianMinutes != nil || row.MultiAttemptShare != nil || row.Over30Share != nil {
			t.Errorf("%s: an empty bucket carries a figure: %+v", label, row)
		}
	}
}

// TestRunawayBeadSizeStatsRefusals drives every branch that must fail the run
// rather than report an empty or clean one.
func TestRunawayBeadSizeStatsRefusals(t *testing.T) {
	t.Parallel()
	fileTown := func(t *testing.T) string {
		t.Helper()
		town := t.TempDir()
		path := filepath.Join(town, ".runtime", "landings")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		return town
	}
	unreadableLedgerTown := func(t *testing.T) string {
		t.Helper()
		town := t.TempDir()
		dir := landReworkStatsTownDir(t, town)
		// A path the reader opens but cannot read: a directory named like a
		// ledger, so the walk does not skip it as a directory entry.
		target := filepath.Join(t.TempDir(), "target")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "gastown.jsonl")); err != nil {
			t.Fatalf("making the unreadable ledger: %v", err)
		}
		return town
	}

	cases := []struct {
		name    string
		find    func(t *testing.T) func() (string, error)
		weeks   int
		wantErr string
	}{
		{
			name:    "weeks below one",
			find:    func(t *testing.T) func() (string, error) { return landReworkStatsTown(t.TempDir()) },
			weeks:   0,
			wantErr: "want at least 1",
		},
		{
			name: "a town that cannot be resolved",
			find: func(*testing.T) func() (string, error) {
				return func() (string, error) { return "", fmt.Errorf("not in a town") }
			},
			weeks:   1,
			wantErr: "resolving the town",
		},
		{
			name:    "a landings path that is a file",
			find:    func(t *testing.T) func() (string, error) { return landReworkStatsTown(fileTown(t)) },
			weeks:   1,
			wantErr: "reading the landings directory",
		},
		{
			name:    "a ledger that will not read",
			find:    func(t *testing.T) func() (string, error) { return landReworkStatsTown(unreadableLedgerTown(t)) },
			weeks:   1,
			wantErr: "gastown.jsonl",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := landSizeStatsReportFrom(tc.find(t), &out, landSizeStatsNow, tc.weeks, false)
			if err == nil {
				t.Fatalf("no error; output:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestRunawayBeadSizeStatsWeeksRefusedBeforeTheTown: a window below one week is
// refused before the command resolves the town or reads a ledger, so a bad
// --weeks fails on the flag, not on the town.
func TestRunawayBeadSizeStatsWeeksRefusedBeforeTheTown(t *testing.T) {
	t.Parallel()
	resolve := func() (string, error) {
		t.Error("--weeks 0 resolved the town")
		return "", errors.New("not in a town")
	}
	var out bytes.Buffer
	err := landSizeStatsReportFrom(resolve, &out, landSizeStatsNow, 0, false)
	if err == nil || !strings.Contains(err.Error(), "want at least 1") {
		t.Fatalf("error = %v, want the --weeks refusal", err)
	}
}

// TestRunawayBeadSizeStatsWeekRefusals: a week the report cannot name stops it,
// whichever week it is, so a partial table never reads as a complete one.
func TestRunawayBeadSizeStatsWeekRefusals(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := landReworkStatsTownDir(t, town)
	writeLandReworkStatsLedger(t, filepath.Join(dir, "gastown.jsonl"), []landings.Record{
		{Bead: "gt-a", Rig: "gastown", LandedAt: landSizeStatsNow, AddedLines: 10, PolecatSecs: 600},
	})
	cases := []struct {
		name    string
		weekOf  func(string) (string, error)
		wantErr string
	}{
		{name: "the current week", weekOf: weekOfOnCall(1, "", errors.New("no clock")), wantErr: "the week of"},
		{name: "the window's first day", weekOf: weekOfOnCall(2, "", errors.New("no clock")), wantErr: "the window's first day"},
		{name: "a landing's week", weekOf: weekOfOnCall(3, "", errors.New("no ledger date")), wantErr: "the week of gt-a's landing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := landSizeStatsFromTown(landReworkStatsTown(town), landSizeStatsNow, 4, tc.weekOf)
			if err == nil {
				t.Fatal("no error: the report named a week it could not read")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestRunawayBeadSizeStatsUnwritableOutput: a report the output cannot take is
// an error in both forms, not a silent success.
func TestRunawayBeadSizeStatsUnwritableOutput(t *testing.T) {
	t.Parallel()
	errDiskFull := errors.New("disk full")
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", asJSON), func(t *testing.T) {
			t.Parallel()
			err := landSizeStatsReportFrom(landReworkStatsTown(t.TempDir()), refusingWriter{err: errDiskFull}, landSizeStatsNow, 1, asJSON)
			if !errors.Is(err, errDiskFull) {
				t.Fatalf("error = %v, want the write's error", err)
			}
		})
	}
}
