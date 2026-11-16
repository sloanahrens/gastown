package reworkstats

import (
	"errors"
	"fmt"
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

// reworkProgramFixture is the record set `gt land rework-stats` prints as
// internal/cmd/testdata/land_rework_stats.golden: a full week, a thin one, a
// full week with no timed landing, a full week with one untimed landing, a
// bead landed twice, and the records the report must count rather than fold
// into a week.
func reworkProgramFixture() ([]landings.Record, []land.ReviewRecord) {
	var recs []landings.Record
	var reviews []land.ReviewRecord
	add := func(rig, bead string, at time.Time, secs float64, seats, rounds int) {
		rec := landings.Record{Bead: bead, Rig: rig, LandedAt: at, PolecatSecs: secs}
		for i := 0; i < seats; i++ {
			rec.PolecatAttempts = append(rec.PolecatAttempts, land.PolecatAttempt{
				Seat: fmt.Sprintf("%s-seat-%d", rig, i), Secs: secs / float64(seats),
			})
		}
		recs = append(recs, rec)
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
		add("gastown", fmt.Sprintf("gt-w1-%02d", i), landing(w1, i), secs, seats, rounds)
	}
	// One bead recorded twice counts once, in the week of its last landing.
	add("gastown", "gt-w1-05", landing(w1, 5).Add(72*time.Hour), 600, 2, 2)

	// 2026-10-26: 4 beads, too few to say.
	w2 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	add("gastown", "gt-w2-00", landing(w2, 0), 600, 1, 2)
	add("gastown", "gt-w2-01", landing(w2, 1), 0, 1, 2)
	add("gastown", "gt-w2-02", landing(w2, 2), 900, 2, 1)
	add("gastown", "gt-w2-03", landing(w2, 3), 1200, 1, 1)

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
		add("longeye", fmt.Sprintf("gt-w3-%02d", i), landing(w3, i), 0, seats, rounds)
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
		add("longeye", fmt.Sprintf("gt-w4-%02d", i), landing(w4, i), secs, seats, rounds)
	}

	// Records the report counts rather than folds into a week.
	add("gastown", "gt-old", time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), 600, 1, 1)
	recs = append(recs, landings.Record{Bead: "gt-notime", Rig: "gastown"})
	recs = append(recs, landings.Record{Rig: "gastown", LandedAt: w4})
	return recs, reviews
}

// TestReworkProgramMatchesTheCommandOutput: the computation returns the numbers
// `gt land rework-stats` printed before the move, week for week, for the
// record set its golden was written from.
func TestReworkProgramMatchesTheCommandOutput(t *testing.T) {
	t.Parallel()
	recs, reviews := reworkProgramFixture()
	report, err := Compute(recs, reviews, reworkProgramNow, 4, metrics.WeekOf)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if report.FirstWeek != "2026-10-19" || report.LastWeek != "2026-11-09" {
		t.Errorf("window = %s to %s, want 2026-10-19 to 2026-11-09", report.FirstWeek, report.LastWeek)
	}
	if report.Outside != 1 || report.NoLandingTime != 1 || report.Skipped != 1 {
		t.Errorf("outside/no-landing-time/skipped = %d/%d/%d, want 1/1/1",
			report.Outside, report.NoLandingTime, report.Skipped)
	}

	share := func(numerator, denominator float64) *float64 {
		v := numerator / denominator
		return &v
	}
	minutes := func(v float64) *float64 { return &v }
	cases := []struct {
		week      string
		beads     int
		twoRounds int
		roundsPct *float64
		twoSeats  int
		seatsPct  *float64
		timed     int
		untimed   int
		mean      *float64
		tooFew    bool
	}{
		{week: "2026-10-19", beads: 24, twoRounds: 8, roundsPct: share(8, 24), twoSeats: 6, seatsPct: share(6, 24), timed: 22, untimed: 2, mean: minutes(10)},
		{week: "2026-10-26", beads: 4, twoRounds: 2, twoSeats: 1, timed: 3, untimed: 1, tooFew: true},
		{week: "2026-11-02", beads: 21, twoRounds: 3, roundsPct: share(3, 21), twoSeats: 2, seatsPct: share(2, 21), timed: 0, untimed: 21},
		{week: "2026-11-09", beads: 22, twoRounds: 5, roundsPct: share(5, 22), twoSeats: 4, seatsPct: share(4, 22), timed: 21, untimed: 1, mean: minutes(20)},
	}
	if len(report.Weeks) != len(cases) {
		t.Fatalf("got %d weeks, want %d", len(report.Weeks), len(cases))
	}
	for i, want := range cases {
		got := report.Weeks[i]
		if got.Week != want.week {
			t.Errorf("week %d = %s, want %s", i, got.Week, want.week)
			continue
		}
		t.Run(want.week, func(t *testing.T) {
			checkReworkProgramWeek(t, got, want.beads, want.twoRounds, want.roundsPct,
				want.twoSeats, want.seatsPct, want.timed, want.untimed, want.mean, want.tooFew)
		})
	}
}

// checkReworkProgramWeek asserts one week's figures, pointer figures included,
// so a nil where a number is due fails rather than reads as zero.
func checkReworkProgramWeek(t *testing.T, got Week, beads, twoRounds int, roundsShare *float64,
	twoSeats int, seatsShare *float64, timed, untimed int, mean *float64, tooFew bool) {
	t.Helper()
	if got.Beads != beads || got.TwoRounds != twoRounds || got.TwoSeats != twoSeats ||
		got.Timed != timed || got.Untimed != untimed || got.TooFew != tooFew {
		t.Errorf("beads/2+rounds/2+seats/timed/untimed/tooFew = %d/%d/%d/%d/%d/%t, want %d/%d/%d/%d/%d/%t",
			got.Beads, got.TwoRounds, got.TwoSeats, got.Timed, got.Untimed, got.TooFew,
			beads, twoRounds, twoSeats, timed, untimed, tooFew)
	}
	checkReworkProgramFigure(t, "two-rounds share", got.TwoRoundsShare, roundsShare)
	checkReworkProgramFigure(t, "two-seats share", got.TwoSeatsShare, seatsShare)
	checkReworkProgramFigure(t, "mean polecat minutes", got.MeanPolecatMinutes, mean)
}

// checkReworkProgramFigure compares a figure a week either gives or does not.
func checkReworkProgramFigure(t *testing.T, name string, got, want *float64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got == nil:
		t.Errorf("%s = none, want %v", name, *want)
	case want == nil:
		t.Errorf("%s = %v, want none", name, *got)
	case *got != *want:
		t.Errorf("%s = %v, want %v", name, *got, *want)
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
