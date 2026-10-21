package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// The plan's months run on the clock the week tests set: claudePlanNow is 16 Oct
// 2026, so the billing month a renewal day of 18 names is 18 Sep to 18 Oct, and
// the month before it is 18 Aug to 18 Sep. Every instant below is UTC, whose
// midnight is the month boundary a test can name.
func planDate(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// planWeek is a closed window of the usual seven days, ending as it resets.
func planWeek(resetsAt time.Time, pct float64) ClaudePlanWeek {
	return ClaudePlanWeek{
		StartAt:  resetsAt.Add(-claudePlanWeekDays * 24 * time.Hour).Unix(),
		ResetsAt: resetsAt.Unix(),
		FinalPct: pct,
	}
}

// planMonthCase is what one month's weeks must come to, with the fields of a
// case that expects nothing left at zero.
type planMonthCase struct {
	name    string
	weeks   []ClaudePlanWeek
	value   float64
	util    float64
	ceiling int
	avg     float64
	count   int
	prev    *float64
	open    bool
}

// The billing month is the plan's own rather than the calendar's, so a weekly
// window can straddle the day the plan renews. It is prorated into both months
// by its days: each month is charged the dollars of the week it held and the two
// add up to the whole week, so no week is counted twice or dropped between them.
func TestComputeClaudePlanMonthProratesAWindowAcrossTheRenewalDay(t *testing.T) {
	t.Parallel()

	pct := func(v float64) *float64 { return &v }
	for _, tt := range []planMonthCase{
		{
			name: "a week wholly inside the month",
			weeks: []ClaudePlanWeek{
				planWeek(planDate(2026, time.September, 27), 70),
			},
			value: 32.24, util: 16.12, avg: 70, count: 1,
		},
		{
			// Four of its seven days are in the month, three in the one before,
			// so $32.24 of week is split $18.42 and $13.82.
			name: "a week the renewal day cuts in two",
			weeks: []ClaudePlanWeek{
				planWeek(planDate(2026, time.September, 22), 70),
			},
			value: 18.42, util: 9.21, avg: 70, count: 1, prev: pct(6.91),
		},
		{
			// The month starts on the instant the window ends, and a window is
			// worth the days it holds: the last of its days is the day before.
			name: "a week that ends where the month starts",
			weeks: []ClaudePlanWeek{
				planWeek(planDate(2026, time.September, 18), 70),
			},
			prev: pct(16.12),
		},
		{
			// The week ends six days into the month, so one of its seven days is
			// in the month before; the other six count here at the ceiling.
			name: "a week one day of which is in the month before",
			weeks: []ClaudePlanWeek{
				planWeek(planDate(2026, time.September, 24), 100),
			},
			value: 39.47, util: 19.74, ceiling: 1, avg: 100, count: 1, prev: pct(3.29),
		},
		{
			name: "a week wholly in the month before",
			weeks: []ClaudePlanWeek{
				planWeek(planDate(2026, time.September, 8), 50),
			},
			prev: pct(11.52),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := ComputeClaudePlanMonth(tt.weeks, claudePlanNow, 200, ClaudePlanDefaultRenewalDay)

			if got := planDate(2026, time.September, 18).Unix(); m.StartAt != got {
				t.Errorf("month_start_at = %d, want the 18th of September (%d)", m.StartAt, got)
			}
			if got := planDate(2026, time.October, 18).Unix(); m.EndAt != got {
				t.Errorf("month_end_at = %d, want the 18th of October (%d)", m.EndAt, got)
			}
			checkPlanMonth(t, m, tt)
		})
	}
}

// A month of weeks: what they come to against the plan's price, how many of them
// ran to the ceiling, and the mean week. A week the history has no row for is
// missing from the count rather than read as a week at zero, and a month with no
// weeks at all reports that rather than a month nobody spent.
func TestComputeClaudePlanMonthCountsTheFixtureMonth(t *testing.T) {
	t.Parallel()

	prev := 11.52
	// The week before this billing month, then four of its own: two at the
	// ceiling, and the last still being spent, which counts at its latest
	// reading rather than at a closeout.
	before := planWeek(planDate(2026, time.August, 27), 50)
	w1 := planWeek(planDate(2026, time.September, 26), 95)
	w2 := planWeek(planDate(2026, time.October, 3), 40)
	w3 := planWeek(planDate(2026, time.October, 10), 100)
	open := planWeek(planDate(2026, time.October, 17), 12)
	open.InProgress = true

	for _, tt := range []planMonthCase{
		{
			name:  "the fixture month",
			weeks: []ClaudePlanWeek{before, w1, w2, w3, open},
			value: 113.74, util: 56.87, ceiling: 2, avg: 61.75, count: 4,
			prev: &prev, open: true,
		},
		{
			// A week the history never got: the dashboard was down, or the
			// samples it would have been folded from have rotated out. The month
			// reports the weeks it has, and the missing one is missing.
			name:  "a month with a missing week",
			weeks: []ClaudePlanWeek{before, w1, w3, open},
			value: 95.32, util: 47.66, ceiling: 2, avg: 69, count: 3,
			prev: &prev, open: true,
		},
		{
			name:  "no history",
			weeks: nil,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkPlanMonth(t, ComputeClaudePlanMonth(tt.weeks, claudePlanNow, 200, ClaudePlanDefaultRenewalDay), tt)
		})
	}
}

// checkPlanMonth is the month's arithmetic, field by field.
func checkPlanMonth(t *testing.T, m ClaudePlanMonth, tt planMonthCase) {
	t.Helper()

	for _, f := range []struct {
		name string
		got  float64
		want float64
	}{
		{"month_value_used_usd", m.ValueUsedUSD, tt.value},
		{"month_utilization_pct", m.UtilizationPct, tt.util},
		{"avg_weekly_pct", m.AvgWeeklyPct, tt.avg},
	} {
		if !near(f.got, f.want) {
			t.Errorf("%s = %v, want %v", f.name, f.got, f.want)
		}
	}
	if m.WeeksAtCeiling != tt.ceiling {
		t.Errorf("weeks_at_ceiling = %d, want %d", m.WeeksAtCeiling, tt.ceiling)
	}
	if m.WeeksCounted != tt.count {
		t.Errorf("month_weeks = %d, want %d", m.WeeksCounted, tt.count)
	}
	if m.OpenWeek != tt.open {
		t.Errorf("month_open_week = %v, want %v", m.OpenWeek, tt.open)
	}
	switch {
	case tt.prev == nil && m.PrevUtilizationPct != nil:
		t.Errorf("prev_month_utilization_pct = %v, want null", *m.PrevUtilizationPct)
	case tt.prev != nil && m.PrevUtilizationPct == nil:
		t.Errorf("prev_month_utilization_pct = null, want %v", *tt.prev)
	case tt.prev != nil && !near(*m.PrevUtilizationPct, *tt.prev):
		t.Errorf("prev_month_utilization_pct = %v, want %v", *m.PrevUtilizationPct, *tt.prev)
	}
}

// The billing month's bounds, case by case: the renewal day at or before now
// and the next one, so the months tile whatever day the operator renews on.
func TestClaudePlanBillingMonthBounds(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		now        time.Time
		renewalDay int
		start, end time.Time
	}{
		{"a mid-month instant", planDate(2026, time.October, 16), 18,
			planDate(2026, time.September, 18), planDate(2026, time.October, 18)},
		{"the renewal day itself belongs to the month it starts",
			planDate(2026, time.September, 18), 18,
			planDate(2026, time.September, 18), planDate(2026, time.October, 18)},
		{"the instant before it belongs to the month before",
			planDate(2026, time.September, 17), 18,
			planDate(2026, time.August, 18), planDate(2026, time.September, 18)},
		{"the first of the month", planDate(2026, time.October, 16), 1,
			planDate(2026, time.October, 1), planDate(2026, time.November, 1)},
		{"the last of the month in a month too short for it",
			planDate(2026, time.February, 20), 31,
			planDate(2026, time.January, 31), planDate(2026, time.February, 28)},
		{"the month after one that was too short",
			planDate(2026, time.March, 5), 31,
			planDate(2026, time.February, 28), planDate(2026, time.March, 31)},
		{"the last of the month in a leap February",
			planDate(2028, time.February, 10), 29,
			planDate(2028, time.January, 29), planDate(2028, time.February, 29)},
		{"a day no month has falls back to the default",
			planDate(2026, time.October, 16), 0,
			planDate(2026, time.September, 18), planDate(2026, time.October, 18)},
		{"a day past the end of every month falls back to the default",
			planDate(2026, time.October, 16), 32,
			planDate(2026, time.September, 18), planDate(2026, time.October, 18)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start, end := claudePlanBillingMonth(tt.now, tt.renewalDay)
			if !start.Equal(tt.start) || !end.Equal(tt.end) {
				t.Errorf("the billing month of %s on renewal day %d = %s to %s, want %s to %s",
					tt.now.Format(time.RFC3339), tt.renewalDay,
					start.Format(time.RFC3339), end.Format(time.RFC3339),
					tt.start.Format(time.RFC3339), tt.end.Format(time.RFC3339))
			}
			// One month's end is the next one's start, which is what makes the
			// trend a comparison of months rather than of overlapping spans.
			if prev, _ := claudePlanBillingMonth(start.Add(-time.Second), tt.renewalDay); !prev.Before(start) {
				t.Errorf("the month before %s starts at %s, at or after it", start.Format(time.RFC3339), prev.Format(time.RFC3339))
			}
		})
	}
}

// The month's fields reach the page flat on the payload and under the names the
// reader reports them by, since the line's suffix and its hover read them off
// State.ClaudePlan's own JSON.
func TestClaudePlanMonthReachesThePayload(t *testing.T) {
	t.Parallel()

	// The week the samples are still spending: the payload that carries an open
	// month is the one the page draws its suffix from.
	open := planWeek(planDate(2026, time.October, 19), 95)
	open.InProgress = true
	body, err := json.Marshal(ClaudePlan{ClaudePlanMonth: ComputeClaudePlanMonth(
		[]ClaudePlanWeek{open}, claudePlanNow, 200, 18)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"month_start_at"`, `"month_end_at"`, `"month_value_used_usd"`, `"month_utilization_pct"`,
		`"weeks_at_ceiling"`, `"avg_weekly_pct"`, `"month_weeks"`, `"month_open_week"`,
		`"prev_month_utilization_pct"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the payload carries no %s: %s", want, body)
		}
	}
	// The month is a picture so far, not a closed account, and the page says so
	// from this field alone.
	if !strings.Contains(string(body), `"month_open_week":true`) {
		t.Errorf("the payload does not mark the month open: %s", body)
	}
}

// planWeeksFile is the history beside a series, decoded as the reader decodes
// it: the rows written, in the order they were written.
func planWeeksFile(t *testing.T, samplesPath string) []ClaudePlanWeek {
	t.Helper()
	weeks, err := readClaudePlanWeeks(claudePlanWeeksPath(samplesPath))
	if err != nil {
		t.Fatalf("reading the weekly history: %v", err)
	}
	return weeks
}

// A window is recorded when its reset passes, and once: the polls after it meet
// the same window in the samples' tail for days, and every one of them must find
// the row already there rather than append a second. The window still running is
// counted in the month and never written, since its percentage is not final.
func TestClaudePlanReaderRecordsEachWeekOnce(t *testing.T) {
	t.Parallel()

	resetsClosed, resetsOpen := planDate(2026, time.October, 12), planDate(2026, time.October, 19)
	path := claudePlanFile(t,
		planLine(planDate(2026, time.October, 10), 5, 30, resetsClosed.Unix()),
		planLine(planDate(2026, time.October, 11), 5, 44, resetsClosed.Unix()),
		planLine(planDate(2026, time.October, 15), 3, 12, resetsOpen.Unix()),
		planLine(claudePlanNow, 4, 14, resetsOpen.Unix()),
	)
	r := NewClaudePlanReader(path, 200)

	for poll := 0; poll < 3; poll++ {
		p := r.Read(claudePlanNow)
		if p.Unavailable {
			t.Fatalf("poll %d read an unavailable meter: %+v", poll, p)
		}
		// The closed window's last reading is its final percentage, and the open
		// one counts at where the week stands.
		if p.WeeksCounted != 2 || !p.OpenWeek {
			t.Errorf("poll %d counted %d weeks (open %v), want the closed one and the open one",
				poll, p.WeeksCounted, p.OpenWeek)
		}
		// The open window resets after the billing month ends, so the month
		// holds six of its seven days: (44 + 12) of a week's value.
		if !near(p.ValueUsedUSD, 25.79) {
			t.Errorf("poll %d: month_value_used_usd = %v, want 25.79", poll, p.ValueUsedUSD)
		}
		if weeks := planWeeksFile(t, path); len(weeks) != 1 {
			t.Fatalf("poll %d left %d history rows, want the closed window's one: %+v", poll, len(weeks), weeks)
		}
	}

	rows := planWeeksFile(t, path)
	want := ClaudePlanWeek{
		StartAt:  resetsClosed.Add(-claudePlanWeekDays * 24 * time.Hour).Unix(),
		ResetsAt: resetsClosed.Unix(),
		FinalPct: 44,
	}
	if rows[0] != want {
		t.Errorf("the history row is %+v, want %+v", rows[0], want)
	}
}

// A window whose reset instant the API reports a few seconds apart is one
// window, in the history as in the pace: two polls that disagree by seconds must
// not record the same week twice.
func TestClaudePlanReaderRecordsAWindowOnceAcrossItsCountdowns(t *testing.T) {
	t.Parallel()

	resets := planDate(2026, time.October, 12)
	path := claudePlanFile(t,
		planLine(planDate(2026, time.October, 11), 5, 44, resets.Unix()-4),
		planLine(claudePlanNow, 5, 47, resets.Unix()),
	)
	r := NewClaudePlanReader(path, 200)

	for poll := 0; poll < 2; poll++ {
		r.Read(claudePlanNow)
		if weeks := planWeeksFile(t, path); len(weeks) != 1 {
			t.Fatalf("poll %d left %d history rows for one window, want 1: %+v", poll, len(weeks), weeks)
		}
	}
}

// A poll that finds the series' oldest line past the retention and its slack
// rewrites the file without the lines older than the retention, by a rename
// rather than in place.
func TestClaudePlanReaderTrimsTheSeriesToNinetyDays(t *testing.T) {
	t.Parallel()

	cutoff := claudePlanNow.Add(-claudePlanRetentionDays * 24 * time.Hour)
	slack := claudePlanTrimSlackDays * 24 * time.Hour
	resets := planDate(2026, time.October, 19).Unix()
	keep := func(at time.Time, pct float64) string { return planLine(at, 5, pct, resets) }
	path := claudePlanFile(t,
		keep(cutoff.Add(-slack-25*time.Hour), 10),
		keep(cutoff.Add(-time.Hour), 20),
		keep(claudePlanNow.Add(-48*time.Hour), 30),
		keep(claudePlanNow, 38),
	)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	p := NewClaudePlanReader(path, 200).Read(claudePlanNow)
	if p.Unavailable || p.WeeklyPct != 38 {
		t.Fatalf("the trimmed series read as %+v, want the newest line at 38%%", p)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the series holds %d lines after the trim, want the two inside the retention: %q",
			len(lines), body)
	}
	for _, gone := range []string{"\"seven_day\":10", "\"seven_day\":20"} {
		if strings.Contains(string(body), gone) {
			t.Errorf("the trim left a line older than the retention behind: %s", body)
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the trim rewrote the series in place: a concurrent reader can see it truncated")
	}
	if before.Mode() != after.Mode() {
		t.Errorf("the trimmed series is %v, want the series' own %v", after.Mode(), before.Mode())
	}
	// Only the series and the history may sit beside it: no temp file survives.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) && e.Name() != claudePlanWeeksFile {
			t.Errorf("the trim left %q beside the series", e.Name())
		}
	}

	NewClaudePlanReader(path, 200).Read(claudePlanNow)
	again, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(after, again) {
		t.Error("a series with nothing old left in it was rewritten")
	}
}

// A line past the retention but within the slack is left alone, so the rewrite
// runs about once a week and not on every poll once the series is full.
func TestClaudePlanReaderLeavesLinesInsideTheTrimSlack(t *testing.T) {
	t.Parallel()

	cutoff := claudePlanNow.Add(-claudePlanRetentionDays * 24 * time.Hour)
	resets := planDate(2026, time.October, 19).Unix()
	path := claudePlanFile(t,
		planLine(cutoff.Add(-24*time.Hour), 5, 10, resets),
		planLine(claudePlanNow, 5, 38, resets),
	)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	NewClaudePlanReader(path, 200).Read(claudePlanNow)

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("a series one day past the retention was rewritten inside the slack")
	}
}

// A line the status line appends to the old file while the trim is rewriting it
// is carried into the new one rather than lost with the old inode.
func TestTrimClaudePlanSamplesKeepsAnAppendMadeDuringTheRewrite(t *testing.T) {
	t.Parallel()

	cutoff := claudePlanNow.Add(-claudePlanRetentionDays * 24 * time.Hour)
	resets := planDate(2026, time.October, 19).Unix()
	path := claudePlanFile(t,
		planLine(cutoff.Add(-claudePlanTrimSlackDays*24*time.Hour-time.Hour), 5, 10, resets),
		planLine(claudePlanNow.Add(-time.Hour), 5, 30, resets),
	)
	racing := planLine(claudePlanNow, 5, 38, resets)

	err := trimClaudePlanSamplesHook(path, cutoff, func() {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		if _, err := f.WriteString(racing + "\n"); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	samples, err := readClaudePlanSamples(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || samples[0].SevenDay != 30 || samples[1].SevenDay != 38 {
		t.Errorf("the trimmed series holds %+v, want the line kept and the line appended mid-rewrite", samples)
	}
}

// A closeout that cannot be written is reported on the payload and leaves the
// week the poll drew as it was.
func TestClaudePlanReaderReportsAStateWriteThatFailed(t *testing.T) {
	t.Parallel()

	resets := planDate(2026, time.October, 12)
	path := claudePlanFile(t,
		planLine(planDate(2026, time.October, 11), 5, 44, resets.Unix()),
		planLine(claudePlanNow, 4, 14, planDate(2026, time.October, 19).Unix()),
	)
	// A directory where the history file belongs makes the closeout append fail.
	if err := os.Mkdir(claudePlanWeeksPath(path), 0o755); err != nil {
		t.Fatal(err)
	}

	p := NewClaudePlanReader(path, 200).Read(claudePlanNow)

	if !strings.Contains(p.StateError, "weekly history") {
		t.Errorf("state_error = %q, want it to name the weekly history", p.StateError)
	}
	if p.Unavailable || p.WeeklyPct != 14 {
		t.Errorf("a failed state write changed the week: %+v", p)
	}

	clean := NewClaudePlanReader(claudePlanFile(t, planLine(claudePlanNow, 4, 14, planDate(2026, time.October, 19).Unix())), 200).Read(claudePlanNow)
	if clean.StateError != "" {
		t.Errorf("a poll with nothing to write reported %q", clean.StateError)
	}
}

// Two closeouts for one window, as two pollers can leave, count as one week.
func TestReadClaudePlanWeeksCountsADuplicatedCloseoutOnce(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), claudePlanWeeksFile)
	week := planWeek(planDate(2026, time.October, 12), 95)
	twin := week
	twin.ResetsAt += 3
	if err := appendClaudePlanWeeks(path, []ClaudePlanWeek{week, twin}); err != nil {
		t.Fatal(err)
	}

	weeks, err := readClaudePlanWeeks(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(weeks) != 1 {
		t.Errorf("the history reads as %d weeks, want the one window: %+v", len(weeks), weeks)
	}
}

// Concurrent reads of one reader record a closed window once.
func TestClaudePlanReaderRecordsAWindowOnceAcrossConcurrentReads(t *testing.T) {
	t.Parallel()

	resets := planDate(2026, time.October, 12)
	path := claudePlanFile(t,
		planLine(planDate(2026, time.October, 11), 5, 44, resets.Unix()),
		planLine(claudePlanNow, 4, 14, planDate(2026, time.October, 19).Unix()),
	)
	r := NewClaudePlanReader(path, 200)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Read(claudePlanNow)
		}()
	}
	wg.Wait()

	body, err := os.ReadFile(claudePlanWeeksPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if rows := strings.Count(string(body), "\n"); rows != 1 {
		t.Errorf("the history holds %d rows after concurrent reads, want 1: %s", rows, body)
	}
}

// A month the history holds but the series has aged past is still reported: the
// weekly rows are what the rollup reads, and the samples are only where they
// come from.
func TestClaudePlanReaderKeepsTheMonthsTheSeriesHasForgotten(t *testing.T) {
	t.Parallel()

	resets := planDate(2026, time.October, 12)
	path := claudePlanFile(t, planLine(claudePlanNow, 4, 14, planDate(2026, time.October, 19).Unix()))
	if err := appendClaudePlanWeeks(claudePlanWeeksPath(path), []ClaudePlanWeek{
		{StartAt: resets.Add(-claudePlanWeekDays * 24 * time.Hour).Unix(), ResetsAt: resets.Unix(), FinalPct: 95},
	}); err != nil {
		t.Fatal(err)
	}

	p := NewClaudePlanReader(path, 200).Read(claudePlanNow)
	if p.WeeksAtCeiling != 1 || p.WeeksCounted != 2 {
		t.Errorf("the read counted %d weeks with %d at the ceiling, want the history's 95%% week and the open one",
			p.WeeksCounted, p.WeeksAtCeiling)
	}
	if p.AvgWeeklyPct != 54.5 {
		t.Errorf("avg_weekly_pct = %v, want the mean of the history's 95 and the open 14", p.AvgWeeklyPct)
	}
}

// A poll that first meets a reset after the tail has grown past the readings
// that named the week before it still closes that week: its bounds are the
// newest reset less a week, and its last percentage the newest reading from
// before its reset, so a reset nothing polled across leaves no hole in the month
// (gt-qt4os).
func TestClaudePlanCloseoutSurvivesAGap(t *testing.T) {
	t.Parallel()

	open := planDate(2026, time.October, 19).Unix()
	carried := planDate(2026, time.October, 12)
	for _, tt := range []struct {
		name    string
		samples []string
		wantPct float64
	}{
		{
			// Every reading the tail still holds came after its reset, so the
			// week's last reading is gone. The reading after the reset is the next
			// week's, so the carried week records 0 rather than that value.
			name: "the tail has outgrown the week's readings",
			samples: []string{
				planLine(planDate(2026, time.October, 14), 4, 12, open),
				planLine(claudePlanNow, 4, 14, open),
			},
			wantPct: 0,
		},
		{
			// A reading from before the reset survives, so it is the week's last.
			name: "a reading from before the reset survives",
			samples: []string{
				planLine(planDate(2026, time.October, 4), 3, 80, planDate(2026, time.October, 5).Unix()),
				planLine(planDate(2026, time.October, 14), 4, 12, open),
				planLine(claudePlanNow, 4, 14, open),
			},
			wantPct: 80,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := claudePlanFile(t, tt.samples...)
			if err := appendClaudePlanWeeks(claudePlanWeeksPath(path), []ClaudePlanWeek{
				planWeek(planDate(2026, time.September, 28), 70),
				planWeek(planDate(2026, time.October, 5), 66),
			}); err != nil {
				t.Fatal(err)
			}

			r := NewClaudePlanReader(path, 200)
			p := r.Read(claudePlanNow)
			if p.Unavailable {
				t.Fatalf("the poll read an unavailable meter: %+v", p)
			}

			want := ClaudePlanWeek{
				StartAt:  carried.Add(-claudePlanWeekDays * 24 * time.Hour).Unix(),
				ResetsAt: carried.Unix(),
				FinalPct: tt.wantPct,
			}
			rows := planWeeksFile(t, path)
			got, ok := ClaudePlanWeek{}, false
			for _, w := range rows {
				if claudePlanSameWindow(w.ResetsAt, want.ResetsAt) {
					got, ok = w, true
				}
			}
			if !ok {
				t.Fatalf("the history holds no week for the reset the tail outgrew, want %+v: %+v", want, rows)
			}
			if got != want {
				t.Errorf("the carried week is %+v, want %+v", got, want)
			}
			// The month counts it: the two recorded weeks, the carried one and
			// the open week.
			if p.WeeksCounted != 4 {
				t.Errorf("month_weeks = %d, want the carried week counted with the other three", p.WeeksCounted)
			}

			// The next poll finds the carry recorded and adds it no second time.
			r.Read(claudePlanNow)
			if again := planWeeksFile(t, path); len(again) != len(rows) {
				t.Errorf("a second poll left %d rows, want the %d the first wrote: %+v", len(again), len(rows), again)
			}
		})
	}
}

// Every week a poll missed is closed, oldest first, each at the last reading the
// tail holds from before its reset; the carry stops at the history's last week.
func TestClaudePlanReaderCarriesEveryMissedWeek(t *testing.T) {
	t.Parallel()

	open := planDate(2026, time.October, 19).Unix()
	path := claudePlanFile(t,
		planLine(planDate(2026, time.September, 26), 3, 50, planDate(2026, time.September, 28).Unix()),
		planLine(planDate(2026, time.October, 14), 4, 12, open),
		planLine(claudePlanNow, 4, 14, open),
	)
	if err := appendClaudePlanWeeks(claudePlanWeeksPath(path), []ClaudePlanWeek{
		planWeek(planDate(2026, time.September, 21), 70),
	}); err != nil {
		t.Fatal(err)
	}

	r := NewClaudePlanReader(path, 200)
	r.Read(claudePlanNow)
	want := []ClaudePlanWeek{
		planWeek(planDate(2026, time.September, 21), 70),
		planWeek(planDate(2026, time.September, 28), 50),
		planWeek(planDate(2026, time.October, 5), 50),
		planWeek(planDate(2026, time.October, 12), 50),
	}
	if got := planWeeksFile(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("the history holds %+v, want %+v", got, want)
	}

	r.Read(claudePlanNow)
	if again := planWeeksFile(t, path); len(again) != len(want) {
		t.Errorf("a second poll left %d rows, want the %d the first wrote", len(again), len(want))
	}
}

// The carry leaves a window alone when its reset has not passed, when a sample
// already names it, or when the history already records it; with an empty
// history it carries nothing.
func TestClaudePlanCarryGuards(t *testing.T) {
	t.Parallel()

	oct := func(d int) time.Time { return planDate(2026, time.October, d) }
	for _, tt := range []struct {
		name    string
		history []ClaudePlanWeek
		samples []string
		want    []ClaudePlanWeek
	}{
		{
			// Nothing is recorded yet, so there is no hole to fill.
			name: "an empty history carries nothing",
			samples: []string{
				planLine(oct(14), 4, 12, oct(19).Unix()),
				planLine(claudePlanNow, 4, 14, oct(19).Unix()),
			},
		},
		{
			// The newest window's reset is still ahead, so only the week before it
			// closes; the reset ahead is never recorded.
			name:    "a reset still ahead is not closed",
			history: []ClaudePlanWeek{planWeek(oct(5), 66)},
			samples: []string{
				planLine(oct(14), 4, 12, oct(26).Unix()),
				planLine(claudePlanNow, 4, 14, oct(26).Unix()),
			},
			want: []ClaudePlanWeek{planWeek(oct(5), 66), planWeek(oct(12), 0)},
		},
		{
			// A sample names the week, so its own last reading is recorded and the
			// carry does not replace it.
			name:    "a week a sample names keeps the sample's reading",
			history: []ClaudePlanWeek{planWeek(oct(5), 66)},
			samples: []string{
				planLine(oct(11), 3, 33, oct(12).Unix()),
				planLine(oct(14), 4, 40, oct(12).Unix()),
				planLine(claudePlanNow, 4, 14, oct(19).Unix()),
			},
			want: []ClaudePlanWeek{planWeek(oct(5), 66), planWeek(oct(12), 40)},
		},
		{
			// The history already records the week, so the poll adds no second row.
			name:    "a week the history records is not carried again",
			history: []ClaudePlanWeek{planWeek(oct(5), 66), planWeek(oct(12), 70)},
			samples: []string{
				planLine(oct(14), 4, 12, oct(19).Unix()),
				planLine(claudePlanNow, 4, 14, oct(19).Unix()),
			},
			want: []ClaudePlanWeek{planWeek(oct(5), 66), planWeek(oct(12), 70)},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := claudePlanFile(t, tt.samples...)
			if len(tt.history) > 0 {
				if err := appendClaudePlanWeeks(claudePlanWeeksPath(path), tt.history); err != nil {
					t.Fatal(err)
				}
			}

			NewClaudePlanReader(path, 200).Read(claudePlanNow)
			if got := planWeeksFile(t, path); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("the history holds %+v, want %+v", got, tt.want)
			}
		})
	}
}
