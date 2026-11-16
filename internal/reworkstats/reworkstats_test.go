package reworkstats

import (
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
	"github.com/steveyegge/gastown/internal/metrics"
)

// reworkProgramNow is the instant every case runs at: a Friday in the week
// starting 2026-11-09, so a four-week window is 10-19 to 11-09.
var reworkProgramNow = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

// reworkProgramGoldenPath is the JSON `gt land rework-stats --json` printed for
// reworkProgramFixture's town before the computation moved here, which
// internal/cmd's TestLandReworkStatsJSON pins. The test reads it rather than
// restating its figures: the golden is the only place a number appears, so a
// figure that drifts reddens both tests.
const reworkProgramGoldenPath = "../cmd/testdata/land_rework_stats.json.golden"

// reworkProgramFixture writes the town the golden was written from — a full
// week, a thin one, a full week with no timed landing, a full week with one
// untimed landing, a bead landed twice, the records the report counts rather
// than folds into a week, and an unparsable line in a ledger and in the review
// log — and returns its landings directory.
func reworkProgramFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "landings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("making the landings directory: %v", err)
	}
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

	writeReworkProgramLog(t, filepath.Join(dir, "gastown.jsonl"), gastown, "this line is not a landing record")
	writeReworkProgramLog(t, filepath.Join(dir, "longeye.jsonl"), longeye)
	writeReworkProgramLog(t, filepath.Join(dir, land.ReviewsFileName), reviews, "this line is not a review record")
	return dir
}

// writeReworkProgramLog writes records as the landing worker writes them, then
// any extra raw lines: the lines a reader must count and not use.
func writeReworkProgramLog[T any](t *testing.T, path string, recs []T, extra ...string) {
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

// TestReworkProgramMatchesTheCommandOutput: reading the town's logs and
// computing the report gives the numbers `gt land rework-stats --json` printed
// before the move, field for field, for the record set its golden was written
// from.
func TestReworkProgramMatchesTheCommandOutput(t *testing.T) {
	t.Parallel()
	dir := reworkProgramFixture(t)
	recs, skipped, err := ReadLandings(dir)
	if err != nil {
		t.Fatalf("reading the landing ledgers: %v", err)
	}
	reviews, unreadable, err := ReadReviews(filepath.Join(dir, land.ReviewsFileName))
	if err != nil {
		t.Fatalf("reading the review log: %v", err)
	}
	report, err := Compute(recs, reviews, reworkProgramNow, 4, metrics.WeekOf)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	// The command folds the lines its readers could not use into the report, so
	// the total matches the one it prints.
	report.Skipped += skipped + unreadable

	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshalling the report: %v", err)
	}
	want, err := os.ReadFile(reworkProgramGoldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v", reworkProgramGoldenPath, err)
	}
	// The golden carries the newline the command's encoder writes after it.
	if string(got) != strings.TrimSuffix(string(want), "\n") {
		t.Errorf("report differs from %s:\ngot:\n%s\nwant:\n%s", reworkProgramGoldenPath, got, want)
	}
}

// reworkProgramWeekOfOnCall is metrics.WeekOf, except that its nth call returns
// week and err instead, so a case picks which week the report cannot name: 1 is
// the current week, 2 the window's first day, 3 the landing's week.
func reworkProgramWeekOfOnCall(n int, week string, err error) func(string) (string, error) {
	calls := 0
	return func(ts string) (string, error) {
		calls++
		if calls == n {
			return week, err
		}
		return metrics.WeekOf(ts)
	}
}

// TestReworkProgramRefusals drives every branch that must fail the run rather
// than report an empty or clean one.
func TestReworkProgramRefusals(t *testing.T) {
	t.Parallel()
	recs := []landings.Record{{Bead: "gt-a", Rig: "gastown", LandedAt: reworkProgramNow}}
	cases := []struct {
		name    string
		weeks   int
		weekOf  func(string) (string, error)
		wantErr string
	}{
		{name: "weeks below one", weeks: 0, weekOf: metrics.WeekOf, wantErr: "want at least 1"},
		{name: "the current week", weeks: 4, weekOf: reworkProgramWeekOfOnCall(1, "", errors.New("no clock")), wantErr: "the week of"},
		{name: "the window's first day", weeks: 4, weekOf: reworkProgramWeekOfOnCall(2, "", errors.New("no clock")), wantErr: "the window's first day"},
		{name: "a landing's week", weeks: 4, weekOf: reworkProgramWeekOfOnCall(3, "", errors.New("no ledger date")), wantErr: "the week of gt-a's landing"},
		{name: "a first week that is not a date", weeks: 4, weekOf: reworkProgramWeekOfOnCall(2, "not-a-day", nil), wantErr: "the window's first week"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Compute(recs, nil, reworkProgramNow, tc.weeks, tc.weekOf); err == nil {
				t.Fatal("no error: the report named a week it could not read")
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}
