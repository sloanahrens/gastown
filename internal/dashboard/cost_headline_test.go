package dashboard

import (
	"strings"
	"testing"
	"time"
)

// claudeHeadlineSamples writes a reading every hour from start through the last
// hour before the fixture's clock, each one point of the weekly percentage
// above the one before, so every step is worth the same known dollars. Hours in
// skip are left out, as a status line that stopped writing them would.
func claudeHeadlineSamples(start time.Time, skip ...time.Time) PlanSamples {
	skipAt := map[int64]bool{}
	for _, at := range skip {
		skipAt[at.Unix()] = true
	}
	var samples []ClaudePlanSample
	pct := 10.0
	for at := start; at.Before(claudeSeriesNow); at = at.Add(time.Hour) {
		if !skipAt[at.Unix()] {
			samples = append(samples, claudeSampleAt(at, pct))
		}
		pct++
	}
	return claudeSeries(samples...)
}

// The headline prices the Claude series into today's dollars and the last 24
// hours' hourly rate: the two figures the DeepSeek tiles beside them answer, so
// a reader sees both spends in the one row (gt-iazpv).
func TestBuildCostChartsHeadlinePricesTodayAndTheLast24Hours(t *testing.T) {
	t.Parallel()

	// A reading every hour from noon the day before the fixture's clock through
	// 16:00, each a point of the week above the last: at $0.4605 a point, every
	// hour is worth 46.05 cents.
	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)
	charts := BuildCostCharts(claudeSeriesNow, nil, nil, 0, claudeSeriesPlan, claudeHeadlineSamples(start))

	h := charts.Headline
	if h == nil {
		t.Fatal("the charts carry no headline")
	}
	if h.Name != claudePlanSeriesName {
		t.Errorf("the headline names the series %q, want the legend's %q", h.Name, claudePlanSeriesName)
	}
	// Today's day bucket holds seventeen steps: 00:00 through 16:00, the first of
	// them from the 23:00 sample the day before.
	if h.Today == nil || *h.Today != 7.83 {
		t.Errorf("today's Claude dollars = %v, want 7.83 (17 points of $0.4605)", ptr(h.Today))
	}
	// The 24-hour window opens at 17:00, whose step from 16:00 it does not own, so
	// 23 of its 24 read hours are priced, at that same rate an hour.
	if h.HoursRead != 24 {
		t.Errorf("hours read = %d, want every hour of the window", h.HoursRead)
	}
	if want := roundTo2(23 * claudeSeriesWeekUSD / 100 / 24); h.PerHour24h == nil || !near(*h.PerHour24h, want) {
		t.Errorf("the last 24 hours' rate = %v, want %.2f (23 points over 24 hours)", ptr(h.PerHour24h), want)
	}
	if h.Why != "" {
		t.Errorf("a series it priced carries the reason it did not: %q", h.Why)
	}
}

// ptr reads a headline figure back for a message without claiming it is set.
func ptr(v *float64) any {
	if v == nil {
		return "no figure"
	}
	return *v
}

// A window the series read only part of prices the rate over the hours it read
// and counts them, rather than dividing by 24 hours it never saw: a rate over a
// window nobody read reads as spend that never happened (gt-iazpv).
func TestBuildCostChartsHeadlineRatesTheHoursItRead(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)
	var skip []time.Time
	for h := 3; h < 9; h++ {
		skip = append(skip, time.Date(2026, 10, 25, h, 0, 0, 0, time.Local))
	}
	h := BuildCostCharts(claudeSeriesNow, nil, nil, 0, claudeSeriesPlan, claudeHeadlineSamples(start, skip...)).Headline
	if h == nil || h.PerHour24h == nil {
		t.Fatalf("the headline has no rate to check: %+v", h)
	}
	if h.HoursRead != 18 {
		t.Errorf("hours read = %d, want the 18 hours the series wrote a sample for", h.HoursRead)
	}
	// The 18 read hours price 16 steps: the window's first hour owns no step, and
	// the six hours with no sample across the gap own none either.
	want := roundTo2(16 * claudeSeriesWeekUSD / 100 / 18)
	if !near(*h.PerHour24h, want) {
		t.Errorf("the rate = %v, want %.2f over the hours it read", *h.PerHour24h, want)
	}
	if twentyFour := roundTo2(16 * claudeSeriesWeekUSD / 100 / 24); near(*h.PerHour24h, twentyFour) {
		t.Errorf("the rate = %.4f divides by 24 hours the series did not read", *h.PerHour24h)
	}
}

// A day the series has no sample for draws no today figure, so the page can
// write a dash rather than the zero of a day nobody measured (gt-iazpv).
func TestBuildCostChartsHeadlineLeavesAnUnreadTodayUnset(t *testing.T) {
	t.Parallel()

	// Two readings the evening before: inside the 24-hour window, before today.
	samples := claudeSeries(
		claudeSampleAt(time.Date(2026, 10, 24, 20, 0, 0, 0, time.Local), 10),
		claudeSampleAt(time.Date(2026, 10, 24, 20, 30, 0, 0, time.Local), 12),
	)
	h := BuildCostCharts(claudeSeriesNow, nil, nil, 0, claudeSeriesPlan, samples).Headline
	if h == nil {
		t.Fatal("the charts carry no headline")
	}
	if h.Today != nil {
		t.Errorf("today = %v, want no figure for a day with no sample", *h.Today)
	}
	if h.PerHour24h == nil {
		t.Error("the window has two readings but the headline priced no rate")
	}
	if h.Why != "" {
		t.Errorf("a series it partly priced carries a reason it drew nothing: %q", h.Why)
	}
}

// A series the charts cannot build draws no figure and says why, so the
// headline never shows the zero of a plan nobody used (gt-iazpv).
func TestBuildCostChartsHeadlineSaysWhyItHasNoClaudeDollars(t *testing.T) {
	t.Parallel()

	old := claudeSampleAt(time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local), 10)
	cases := []struct {
		name    string
		samples PlanSamples
		want    string
	}{
		{"not read yet", PlanSamples{}, "not been read yet"},
		{"unreadable", PlanSamples{Read: true, Failed: true}, "could not be read"},
		{"no sample written", claudeSeries(), "written no sample"},
		{"no sample in the window", claudeSeries(old), "no sample falls in this window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := BuildCostCharts(claudeSeriesNow, nil, nil, 0, claudeSeriesPlan, tc.samples).Headline
			if h == nil {
				t.Fatal("the charts carry no headline")
			}
			if h.Today != nil || h.PerHour24h != nil {
				t.Errorf("a series with no dollars drew some: today %v, rate %v", ptr(h.Today), ptr(h.PerHour24h))
			}
			if !strings.Contains(h.Why, tc.want) {
				t.Errorf("the headline's reason = %q, want it to say %q", h.Why, tc.want)
			}
		})
	}
}
