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

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/metrics"
)

var updateLandReworkStatsGolden = flag.Bool("update-land-rework-stats-golden", false, "rewrite testdata/land_rework_stats.golden and .json.golden")

const (
	landReworkStatsGoldenPath     = "land_rework_stats.golden"
	landReworkStatsJSONGoldenPath = "land_rework_stats.json.golden"
)

// landReworkStatsNow is the instant every case runs at: a Friday in the week
// starting 2026-11-09, so a four-week window is 10-19 to 11-09.
var landReworkStatsNow = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

// landReworkStatsFixture writes a town whose landings cover the four weeks the
// golden reads: a full week, a thin one, a full week with no timed landing, a
// full week with one untimed landing, and the records the report must count
// rather than fold into a week.
func landReworkStatsFixture(t *testing.T) (town, dir string) {
	t.Helper()
	town = t.TempDir()
	dir = landReworkStatsTownDir(t, town)

	var gastown, longeye []landings.Record
	var reviews []land.ReviewRecord
	add := func(file *[]landings.Record, rig, bead string, at time.Time, secs float64, seats, rounds int) {
		rec := landings.Record{Bead: bead, Rig: rig, LandedAt: at, PolecatSecs: secs}
		for i := 0; i < seats; i++ {
			rec.PolecatAttempts = append(rec.PolecatAttempts, land.PolecatAttempt{
				Seat: fmt.Sprintf("%s-seat-%d", rig, i), Secs: secs / float64(seats),
			})
		}
		*file = append(*file, rec)
		for i := 1; i <= rounds; i++ {
			reviews = append(reviews, land.ReviewRecord{
				BeadID: bead, Rig: rig, Attempt: i, Verdict: land.VerdictApprove, At: at.Add(-time.Duration(i) * time.Hour),
			})
		}
	}
	landing := func(week time.Time, i int) time.Time {
		return week.AddDate(0, 0, i%4).Add(time.Duration(9+i%8) * time.Hour)
	}

	// 2026-10-19: 24 beads, 8 with two rounds, 6 with two seats, 2 untimed.
	w1 := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 24; i++ {
		rounds, seats, secs := 1, 1, 600.0
		if i < 8 {
			rounds = 2
		}
		if i < 6 {
			seats = 2
		}
		if i >= 22 {
			secs = 0
		}
		add(&gastown, "gastown", fmt.Sprintf("gt-w1-%02d", i), landing(w1, i), secs, seats, rounds)
	}
	// One bead recorded twice counts once, in the week of its last landing.
	add(&gastown, "gastown", "gt-w1-05", landing(w1, 5).Add(72*time.Hour), 600, 2, 2)

	// 2026-10-26: 4 beads, too few to say.
	w2 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	add(&gastown, "gastown", "gt-w2-00", landing(w2, 0), 600, 1, 2)
	add(&gastown, "gastown", "gt-w2-01", landing(w2, 1), 0, 1, 2)
	add(&gastown, "gastown", "gt-w2-02", landing(w2, 2), 900, 2, 1)
	add(&gastown, "gastown", "gt-w2-03", landing(w2, 3), 1200, 1, 1)

	// 2026-11-02: 21 beads, none with a polecat time.
	w3 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 21; i++ {
		rounds, seats := 1, 1
		if i < 3 {
			rounds = 2
		}
		if i < 2 {
			seats = 2
		}
		add(&longeye, "longeye", fmt.Sprintf("gt-w3-%02d", i), landing(w3, i), 0, seats, rounds)
	}

	// 2026-11-09: 22 beads, 5 with two rounds, 4 with two seats, 1 untimed.
	w4 := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 22; i++ {
		rounds, seats, secs := 1, 1, 1200.0
		if i < 5 {
			rounds = 2
		}
		if i < 4 {
			seats = 2
		}
		if i == 21 {
			secs = 0
		}
		add(&longeye, "longeye", fmt.Sprintf("gt-w4-%02d", i), landing(w4, i), secs, seats, rounds)
	}

	// Records the report counts rather than folds into a week.
	add(&gastown, "gastown", "gt-old", time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), 600, 1, 1)
	gastown = append(gastown, landings.Record{Bead: "gt-notime", Rig: "gastown"})
	gastown = append(gastown, landings.Record{Rig: "gastown", LandedAt: w4})

	writeLandReworkStatsLedger(t, filepath.Join(dir, "gastown.jsonl"), gastown, "this line is not a landing record")
	writeLandReworkStatsLedger(t, filepath.Join(dir, "longeye.jsonl"), longeye)
	writeLandReworkStatsReviews(t, filepath.Join(dir, land.ReviewsFileName), reviews, "this line is not a review record")
	return town, dir
}

// landReworkStatsTownDir makes the town's landings directory and returns it.
func landReworkStatsTownDir(t *testing.T, town string) string {
	t.Helper()
	dir := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("making the landings directory: %v", err)
	}
	return dir
}

// writeLandReworkStatsLedger writes one rig's ledger: the records as the
// landing worker writes them, then any extra raw lines.
func writeLandReworkStatsLedger(t *testing.T, path string, recs []landings.Record, extra ...string) {
	t.Helper()
	var lines []string
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal %+v: %v", rec, err)
		}
		lines = append(lines, string(line))
	}
	lines = append(lines, extra...)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// writeLandReworkStatsReviews writes the review log, then any extra raw lines.
func writeLandReworkStatsReviews(t *testing.T, path string, recs []land.ReviewRecord, extra ...string) {
	t.Helper()
	var lines []string
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal %+v: %v", rec, err)
		}
		lines = append(lines, string(line))
	}
	lines = append(lines, extra...)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// landReworkStatsTown is the town resolution a case runs with.
func landReworkStatsTown(town string) func() (string, error) {
	return func() (string, error) { return town, nil }
}

// checkLandReworkStatsGolden compares out with the golden file, rewriting it
// under the update flag.
func checkLandReworkStatsGolden(t *testing.T, name string, out []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateLandReworkStatsGolden {
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatalf("rewriting %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(out) != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-land-rework-stats-golden):\n%s\nwant:\n%s", path, out, want)
	}
}

// TestLandReworkStats pins the weekly table: the share of beads with two or
// more om rounds, with two or more seats, and the mean polecat minutes, with
// the untimed landings counted separately and the thin week saying so.
func TestLandReworkStats(t *testing.T) {
	t.Parallel()
	town, _ := landReworkStatsFixture(t)
	var out bytes.Buffer
	if err := landReworkStatsReportFrom(landReworkStatsTown(town), &out, landReworkStatsNow, 4, false); err != nil {
		t.Fatal(err)
	}
	checkLandReworkStatsGolden(t, landReworkStatsGoldenPath, out.Bytes())
}

// TestLandReworkStatsJSON pins that --json prints the same numbers, including
// a null for a figure a week cannot give.
func TestLandReworkStatsJSON(t *testing.T) {
	t.Parallel()
	town, _ := landReworkStatsFixture(t)
	var out bytes.Buffer
	if err := landReworkStatsReportFrom(landReworkStatsTown(town), &out, landReworkStatsNow, 4, true); err != nil {
		t.Fatal(err)
	}
	checkLandReworkStatsGolden(t, landReworkStatsJSONGoldenPath, out.Bytes())
}

// landReworkReportOf runs the report over town and decodes its JSON, so a case
// asserts a figure rather than a column's spacing.
func landReworkReportOf(t *testing.T, town string, now time.Time, weeks int) landReworkReport {
	t.Helper()
	var out bytes.Buffer
	if err := landReworkStatsReportFrom(landReworkStatsTown(town), &out, now, weeks, true); err != nil {
		t.Fatalf("the report failed: %v", err)
	}
	var report landReworkReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decoding the report: %v\n%s", err, out.String())
	}
	return report
}

// TestLandReworkStatsUntimedCountedApart: a landing with no polecat time is
// counted in Untimed and kept out of the mean's denominator, so a missing
// measurement never reads as a free bead.
func TestLandReworkStatsUntimedCountedApart(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := landReworkStatsTownDir(t, town)
	week := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	var recs []landings.Record
	for i := 0; i < 20; i++ {
		secs := 600.0
		if i == 19 {
			secs = 0
		}
		recs = append(recs, landings.Record{Bead: fmt.Sprintf("gt-u-%02d", i), Rig: "gastown", LandedAt: week.Add(time.Duration(i) * time.Hour), PolecatSecs: secs})
	}
	writeLandReworkStatsLedger(t, filepath.Join(dir, "gastown.jsonl"), recs)

	report := landReworkReportOf(t, town, landReworkStatsNow, 1)
	if len(report.Weeks) != 1 {
		t.Fatalf("got %d weeks, want 1", len(report.Weeks))
	}
	row := report.Weeks[0]
	if row.Beads != 20 || row.Timed != 19 || row.Untimed != 1 {
		t.Errorf("beads/timed/untimed = %d/%d/%d, want 20/19/1", row.Beads, row.Timed, row.Untimed)
	}
	if row.MeanPolecatMinutes == nil {
		t.Fatal("no mean for a week with 19 timed landings")
	}
	if got := *row.MeanPolecatMinutes; got != 10 {
		t.Errorf("mean = %v minutes, want 10 (the 19 timed at 600s, not the 20th read as zero)", got)
	}
}

// TestLandReworkStatsNoTimedRows: a full week whose every landing lacks a time
// has no mean, and the report says so rather than print 0.
func TestLandReworkStatsNoTimedRows(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := landReworkStatsTownDir(t, town)
	week := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	var recs []landings.Record
	for i := 0; i < 20; i++ {
		recs = append(recs, landings.Record{Bead: fmt.Sprintf("gt-n-%02d", i), Rig: "gastown", LandedAt: week.Add(time.Duration(i) * time.Hour)})
	}
	writeLandReworkStatsLedger(t, filepath.Join(dir, "gastown.jsonl"), recs)

	report := landReworkReportOf(t, town, landReworkStatsNow, 1)
	row := report.Weeks[0]
	if row.TooFew {
		t.Fatal("a 20-bead week read as too few to say")
	}
	if row.MeanPolecatMinutes != nil {
		t.Errorf("mean = %v, want none for a week with no timed landing", *row.MeanPolecatMinutes)
	}
	if row.TwoRoundsShare == nil || *row.TwoRoundsShare != 0 {
		t.Errorf("two-rounds share = %v, want 0 (a share of beads a week with none has)", row.TwoRoundsShare)
	}

	var out bytes.Buffer
	if err := landReworkStatsReportFrom(landReworkStatsTown(town), &out, landReworkStatsNow, 1, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no timed rows") {
		t.Errorf("table does not say the week has no timed rows:\n%s", out.String())
	}
}

// TestLandReworkStatsCountsWhatItCannotUse: records the report cannot fold
// into a week are counted and named, and a bead landed twice counts once.
func TestLandReworkStatsCountsWhatItCannotUse(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := landReworkStatsTownDir(t, town)
	current := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	earlier := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var recs []landings.Record
	for i := 0; i < 20; i++ {
		recs = append(recs, landings.Record{Bead: fmt.Sprintf("gt-c-%02d", i), Rig: "gastown", LandedAt: current.Add(time.Duration(i) * time.Hour), PolecatSecs: 600})
	}
	// The same bead's earlier landing line: the later one is the week it
	// counts in.
	recs = append(recs, landings.Record{Bead: "gt-c-00", Rig: "gastown", LandedAt: earlier, PolecatSecs: 60})
	recs = append(recs, landings.Record{Bead: "gt-notime", Rig: "gastown"})
	recs = append(recs, landings.Record{Rig: "gastown", LandedAt: current})
	recs = append(recs, landings.Record{Bead: "gt-old", Rig: "gastown", LandedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), PolecatSecs: 600})
	writeLandReworkStatsLedger(t, filepath.Join(dir, "gastown.jsonl"), recs, "not a record")

	report := landReworkReportOf(t, town, landReworkStatsNow, 2)
	if report.NoLandingTime != 1 {
		t.Errorf("no_landing_time = %d, want 1", report.NoLandingTime)
	}
	if report.Outside != 1 {
		t.Errorf("outside_window = %d, want 1", report.Outside)
	}
	if report.Skipped != 2 {
		t.Errorf("unreadable_lines = %d, want 2 (one bad line, one record naming no bead)", report.Skipped)
	}
	if len(report.Weeks) != 2 {
		t.Fatalf("got %d weeks, want 2", len(report.Weeks))
	}
	if got := report.Weeks[1].Beads; got != 20 {
		t.Errorf("current week beads = %d, want 20 (the duplicate bead counted once, in its last week)", got)
	}
	if got := report.Weeks[0].Beads; got != 0 {
		t.Errorf("earlier week beads = %d, want 0 (the duplicate bead's last landing moved it)", got)
	}
	if !report.Weeks[0].TooFew {
		t.Error("an empty week is not marked too few to say")
	}
}

// TestLandReworkStatsRefusals drives every branch that must fail the run
// rather than report an empty or clean one.
func TestLandReworkStatsRefusals(t *testing.T) {
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
	unreadableReviewsTown := func(t *testing.T) string {
		t.Helper()
		town := t.TempDir()
		dir := landReworkStatsTownDir(t, town)
		if err := os.MkdirAll(filepath.Join(dir, land.ReviewsFileName), 0o700); err != nil {
			t.Fatal(err)
		}
		return town
	}

	unopenableReviewsTown := func(t *testing.T) string {
		t.Helper()
		town := t.TempDir()
		path := filepath.Join(landReworkStatsTownDir(t, town), land.ReviewsFileName)
		// A link to itself: the open fails with a loop, not a missing file, so
		// the report must name it rather than read it as no records.
		if err := os.Symlink(path, path); err != nil {
			t.Fatalf("making the review log loop: %v", err)
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
		{
			name:    "a review log that will not read",
			find:    func(t *testing.T) func() (string, error) { return landReworkStatsTown(unreadableReviewsTown(t)) },
			weeks:   1,
			wantErr: "reading the review records",
		},
		{
			name:    "a review log that will not open",
			find:    func(t *testing.T) func() (string, error) { return landReworkStatsTown(unopenableReviewsTown(t)) },
			weeks:   1,
			wantErr: "opening the review records",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := landReworkStatsReportFrom(tc.find(t), &out, landReworkStatsNow, tc.weeks, false)
			if err == nil {
				t.Fatalf("no error; output:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// weekOfOnCall is metrics.WeekOf, except that its nth call returns week and err
// instead, so a case picks which week the report cannot name: 1 is the current
// week, 2 the window's first day, 3 the landing's week.
func weekOfOnCall(n int, week string, err error) func(string) (string, error) {
	calls := 0
	return func(ts string) (string, error) {
		calls++
		if calls == n {
			return week, err
		}
		return metrics.WeekOf(ts)
	}
}

// TestLandReworkStatsWeekRefusals: a week the report cannot name stops it,
// whichever week it is, and a first week that is not a date is no window.
func TestLandReworkStatsWeekRefusals(t *testing.T) {
	t.Parallel()
	recs := []landings.Record{{Bead: "gt-a", Rig: "gastown", LandedAt: landReworkStatsNow}}
	cases := []struct {
		name    string
		weekOf  func(string) (string, error)
		wantErr string
	}{
		{name: "the current week", weekOf: weekOfOnCall(1, "", errors.New("no clock")), wantErr: "the week of"},
		{name: "the window's first day", weekOf: weekOfOnCall(2, "", errors.New("no clock")), wantErr: "the window's first day"},
		{name: "a landing's week", weekOf: weekOfOnCall(3, "", errors.New("no ledger date")), wantErr: "the week of gt-a's landing"},
		{name: "a first week that is not a date", weekOf: weekOfOnCall(2, "not-a-day", nil), wantErr: "the window's first week"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := landReworkStatsAggregate(recs, nil, landReworkStatsNow, 4, tc.weekOf)
			if err == nil {
				t.Fatal("no error: the report named a week it could not read")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestLandReworkStatsWeekErrorReachesTheCaller: a week the report cannot name
// is the run's error, not a partial report.
func TestLandReworkStatsWeekErrorReachesTheCaller(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	_, err := landReworkStatsFromTown(landReworkStatsTown(town), landReworkStatsNow, 4, weekOfOnCall(1, "", errors.New("no clock")))
	if err == nil || !strings.Contains(err.Error(), "the week of") {
		t.Fatalf("error = %v, want the week refusal", err)
	}
}

// TestLandReworkStatsUnwritableOutput: a report the output cannot take is an
// error in both forms, not a silent success.
func TestLandReworkStatsUnwritableOutput(t *testing.T) {
	t.Parallel()
	errDiskFull := errors.New("disk full")
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", asJSON), func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			err := landReworkStatsReportFrom(landReworkStatsTown(town), refusingWriter{err: errDiskFull}, landReworkStatsNow, 1, asJSON)
			if !errors.Is(err, errDiskFull) {
				t.Fatalf("error = %v, want the write's error", err)
			}
		})
	}
}

// TestLandReworkStatsEmptyTown: a town that has not landed yet has no
// landings directory, and that is an empty report rather than a failure.
func TestLandReworkStatsEmptyTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out bytes.Buffer
	if err := landReworkStatsReportFrom(landReworkStatsTown(town), &out, landReworkStatsNow, 2, false); err != nil {
		t.Fatalf("the report failed: %v", err)
	}
	if !strings.Contains(out.String(), "rework stats: 2 week(s), 2026-11-02 to 2026-11-09") {
		t.Errorf("output does not name the window:\n%s", out.String())
	}
	if strings.Count(out.String(), "too few to say") != 6 {
		t.Errorf("want both figures of both empty weeks too few to say:\n%s", out.String())
	}
}

// TestLandReworkStatsWritesNoFile: the command is read-only, so a run leaves
// every file under the landings directory as it was.
func TestLandReworkStatsWritesNoFile(t *testing.T) {
	t.Parallel()
	town, dir := landReworkStatsFixture(t)
	before := landReworkStatsTree(t, dir)
	var out bytes.Buffer
	if err := landReworkStatsReportFrom(landReworkStatsTown(town), &out, landReworkStatsNow, 4, false); err != nil {
		t.Fatal(err)
	}
	after := landReworkStatsTree(t, dir)
	if len(after) != len(before) {
		t.Fatalf("the directory holds %d entries after the run, %d before", len(after), len(before))
	}
	for name, was := range before {
		if after[name] != was {
			t.Errorf("%s changed: %q, then %q", name, was, after[name])
		}
	}
}

// landReworkStatsTree snapshots every file under dir by its size and bytes.
func landReworkStatsTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	tree := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		tree[entry.Name()] = fmt.Sprintf("%d %s", len(data), data)
	}
	return tree
}
