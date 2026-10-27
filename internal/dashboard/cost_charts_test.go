package dashboard

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// costChartsPerDay is nine days of spend, so the daily chart keeps the last
// seven; 2026-10-22 is a top-up (a negative spend).
var costChartsNow = time.Date(2026, 10, 25, 16, 30, 0, 0, time.Local)

var costChartsPerDay = map[string]float64{
	"2026-10-17": 1.00,
	"2026-10-18": 2.00,
	"2026-10-19": 1.50,
	"2026-10-20": 0.50,
	"2026-10-21": 3.00,
	"2026-10-22": -20.00,
	"2026-10-23": 2.25,
	"2026-10-24": 1.75,
	"2026-10-25": 4.00,
}

// costChartsHours is one day of hours from 2026-10-24 17:00 local, with a gap
// at 20:00 (no ledger sample) and spend at 18:00 and 19:00.
func costChartsHours() []SpendHour {
	first := time.Date(2026, 10, 24, 17, 0, 0, 0, time.Local)
	hours := make([]SpendHour, spendHours)
	for i := range hours {
		hours[i] = SpendHour{Start: first.Add(time.Duration(i) * time.Hour), Samples: 2}
	}
	hours[1].Spent = 0.40 // 18:00
	hours[2].Spent = 0.10 // 19:00
	hours[3] = SpendHour{Start: first.Add(3 * time.Hour)}
	return hours
}

func TestBuildCostChartsDailyKeepsTheLastSevenDaysAsBars(t *testing.T) {
	t.Parallel()
	c := BuildCostCharts(costChartsNow, costChartsPerDay, nil, 0, nil, PlanSamples{}).Daily
	if c == nil {
		t.Fatal("no daily chart from nine days of spend")
	}
	if len(c.Bars) != 7 {
		t.Fatalf("daily bars = %d, want 7", len(c.Bars))
	}
	if c.Bars[0].Label != "10-19" || c.Bars[6].Label != "10-25" {
		t.Errorf("daily labels run %q to %q, want 10-19 to 10-25", c.Bars[0].Label, c.Bars[6].Label)
	}
}

func TestBuildCostChartsDailyDrawsATopUpAsNoSpend(t *testing.T) {
	t.Parallel()
	c := BuildCostCharts(costChartsNow, costChartsPerDay, nil, 0, nil, PlanSamples{}).Daily
	// 2026-10-22 is inside the window and is a top-up: its bar is zero and its
	// hover says money was added, not spent.
	var topUp *CostBar
	for i := range c.Bars {
		if c.Bars[i].Label == "10-22" {
			topUp = &c.Bars[i]
		}
	}
	if topUp == nil {
		t.Fatal("the top-up day is not in the chart")
	}
	if topUp.Value != 0 || !strings.Contains(topUp.Title, "added") {
		t.Errorf("top-up bar = %+v, want a zero bar titled as money added", *topUp)
	}
}

func TestBuildCostChartsDailyDrawsTheWeeklyLimitAsALine(t *testing.T) {
	t.Parallel()
	plan := &ClaudePlan{WeeklyValueUSD: 70, ProjectedPctAtReset: 83.4}
	c := BuildCostCharts(costChartsNow, costChartsPerDay, nil, 0, plan, PlanSamples{}).Daily
	if c.Limit == nil {
		t.Fatal("no limit line with a weekly value")
	}
	if math.Abs(c.Limit.Value-10) > 1e-9 {
		t.Errorf("limit = %v, want 10 a day (70 over seven days)", c.Limit.Value)
	}
	for _, want := range []string{"weekly limit $10.00/day", "reset at 83%"} {
		if !strings.Contains(c.Limit.Label, want) {
			t.Errorf("limit label %q does not contain %q", c.Limit.Label, want)
		}
	}
	if !strings.Contains(c.Aria, "Projected at reset 83% of the weekly limit") {
		t.Errorf("aria %q does not carry the projected value", c.Aria)
	}
}

func TestBuildCostChartsDailyHasNoLimitWithoutAPlan(t *testing.T) {
	t.Parallel()
	if c := BuildCostCharts(costChartsNow, costChartsPerDay, nil, 0, nil, PlanSamples{}).Daily; c.Limit != nil {
		t.Errorf("limit = %+v with no plan, want none", c.Limit)
	}
	if c := BuildCostCharts(costChartsNow, costChartsPerDay, nil, 0, &ClaudePlan{}, PlanSamples{}).Daily; c.Limit != nil {
		t.Errorf("limit = %+v with a zero weekly value, want none", c.Limit)
	}
}

func TestBuildCostChartsHourlyHasOneBarPerHourAndGapsAreNotZeroBars(t *testing.T) {
	t.Parallel()
	c := BuildCostCharts(costChartsNow, nil, costChartsHours(), 0, nil, PlanSamples{}).Hourly
	if c == nil {
		t.Fatal("no hourly chart from 24 hours")
	}
	if len(c.Bars) != spendHours {
		t.Fatalf("hourly bars = %d, want %d", len(c.Bars), spendHours)
	}
	gaps := 0
	for _, b := range c.Bars {
		if b.Gap {
			gaps++
			if b.Value != 0 {
				t.Errorf("gap %s has value %v, want none", b.Label, b.Value)
			}
		}
	}
	if gaps != 1 {
		t.Errorf("gaps = %d, want the one hour with no sample", gaps)
	}
	if !strings.Contains(c.Aria, "1 hour has no ledger sample") {
		t.Errorf("aria %q does not name the gap", c.Aria)
	}
}

func TestBuildCostChartsLeavesAChartOutWithNoData(t *testing.T) {
	t.Parallel()
	c := BuildCostCharts(costChartsNow, nil, nil, 0, nil, PlanSamples{})
	if c.Daily != nil || c.Hourly != nil {
		t.Errorf("charts with no data = %+v, want neither", c)
	}
}

func TestSpendPerDayReadsTheReportAndSkipsAnUnsetDay(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"per_day":{"2026-10-24":{"spent":1.5},"2026-10-25":{"spent":null}}}`)
	got := SpendPerDay(raw)
	if got["2026-10-24"] != 1.5 {
		t.Errorf("2026-10-24 = %v, want 1.5", got["2026-10-24"])
	}
	if _, ok := got["2026-10-25"]; ok {
		t.Error("an unset day was read as a figure")
	}
	if SpendPerDay(nil) != nil {
		t.Error("an absent report is not nil")
	}
}

// The daily chart is always seven calendar days ending today: a day with no
// figure is a gap, so a fortnight of two-day history still draws seven bars
// (gt-idfec).
func TestBuildCostChartsDailyIsSevenDaysWithGapsWhenFewerExist(t *testing.T) {
	t.Parallel()
	perDay := map[string]float64{"2026-10-24": 1.5, "2026-10-25": 2.0}
	c := BuildCostCharts(costChartsNow, perDay, nil, 0, &ClaudePlan{WeeklyValueUSD: 70}, PlanSamples{}).Daily
	if c == nil || len(c.Bars) != costDailyDays {
		t.Fatalf("daily bars = %d, want seven calendar days", len(c.Bars))
	}
	gaps := 0
	for _, b := range c.Bars {
		if b.Gap {
			gaps++
		}
	}
	if gaps != costDailyDays-2 {
		t.Errorf("gaps = %d, want the five days with no figure", gaps)
	}
	if c.Bars[costDailyDays-1].Label != "10-25" || c.Bars[costDailyDays-2].Label != "10-24" {
		t.Errorf("the last bars run %q, %q; want the two known days ending today",
			c.Bars[costDailyDays-2].Label, c.Bars[costDailyDays-1].Label)
	}
	if c.Limit == nil {
		t.Error("no weekly limit line with a plan that names a weekly value")
	}
}

// Malformed ledger lines the read skipped reach the hourly chart: counted on it
// and named in its aria-label (gt-idfec).
func TestBuildCostChartsReportsTheSkippedLedgerLines(t *testing.T) {
	t.Parallel()
	c := BuildCostCharts(costChartsNow, nil, costChartsHours(), 2, nil, PlanSamples{}).Hourly
	if c == nil || c.Skipped != 2 {
		t.Fatalf("hourly skipped = %+v, want 2 on the chart", c)
	}
	if !strings.Contains(c.Aria, "2 malformed ledger lines skipped") {
		t.Errorf("aria %q does not name the skipped lines", c.Aria)
	}
}

// A projection rounds the same way on either side of zero, half away from zero.
func TestPctTextRoundsHalfAwayFromZero(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   float64
		want string
	}{{82.5, "83%"}, {82.4, "82%"}, {-0.5, "-1%"}, {-0.4, "0%"}, {0, "0%"}} {
		if got := pctText(tc.in); got != tc.want {
			t.Errorf("pctText(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The hub rebuilds the Cost panel's charts on every spend poll, so the hours
// that move while the spend report fails still reach the page, and the daily
// chart keeps its last good figures.
func TestPollSpendRebuildsTheChartsOnEveryPoll(t *testing.T) {
	t.Parallel()
	first := costChartsHours()
	second := costChartsHours()
	second[1].Spent = 0.90 // the 18:00 hour grows between polls
	hours := first
	h := NewHub(Config{
		Spend:      func() json.RawMessage { return nil },
		SpendHours: func() SpendRead { return SpendRead{Hours: hours} },
		SpendEvery: time.Minute,
	})
	h.pollSpend()
	hours = second
	h.pollSpend()

	h.mu.Lock()
	charts := h.state.CostCharts
	h.mu.Unlock()
	if charts == nil || charts.Hourly == nil {
		t.Fatal("no hourly chart after two polls with a failed spend report")
	}
	if got := charts.Hourly.Bars[1].Value; got != 0.90 {
		t.Errorf("18:00 bar = %v after the second poll, want the new 0.90", got)
	}
}

// A projection of exactly 0% is a figure, so the limit label names it.
func TestBuildCostChartsDailyNamesAZeroProjection(t *testing.T) {
	t.Parallel()
	c := BuildCostCharts(costChartsNow, costChartsPerDay, nil, 0, &ClaudePlan{WeeklyValueUSD: 70, ProjectedPctAtReset: 0}, PlanSamples{}).Daily
	if c.Limit == nil || !strings.Contains(c.Limit.Label, "reset at 0%") {
		t.Errorf("limit label = %+v, want it to name a 0%% projection", c.Limit)
	}
}

// A ledger read that fails drops the hourly chart: the read has no hours, so the
// chart is absent rather than drawing the hours of an earlier poll as current.
func TestPollSpendDropsTheHoursWhenTheLedgerReadFails(t *testing.T) {
	t.Parallel()
	reads := []SpendRead{{Hours: costChartsHours()}, {}}
	h := NewHub(Config{
		Spend:      func() json.RawMessage { return nil },
		SpendHours: func() SpendRead { r := reads[0]; reads = reads[1:]; return r },
		SpendEvery: time.Minute,
	})
	h.pollSpend()
	h.pollSpend()

	h.mu.Lock()
	charts := h.state.CostCharts
	h.mu.Unlock()
	if charts == nil {
		t.Fatal("no charts after a failed ledger read")
	}
	if charts.Hourly != nil {
		t.Errorf("hourly chart = %+v after a failed read, want it absent", charts.Hourly)
	}
}

// The skipped count from the ledger read reaches the hourly chart on the hub.
func TestPollSpendCarriesTheSkippedLedgerLines(t *testing.T) {
	t.Parallel()
	h := NewHub(Config{
		Spend:      func() json.RawMessage { return nil },
		SpendHours: func() SpendRead { return SpendRead{Hours: costChartsHours(), Skipped: 3} },
		SpendEvery: time.Minute,
	})
	h.pollSpend()

	h.mu.Lock()
	charts := h.state.CostCharts
	h.mu.Unlock()
	if charts == nil || charts.Hourly == nil || charts.Hourly.Skipped != 3 {
		t.Fatalf("hourly skipped = %+v, want 3 from the read", charts)
	}
}

// A plan poll rebuilds the charts too, so the weekly limit line reaches the page
// once the plan has reported.
func TestPollClaudePlanRebuildsTheLimitLine(t *testing.T) {
	t.Parallel()
	perDay := json.RawMessage(`{"per_day":{"2026-10-24":{"spent":1.5},"2026-10-25":{"spent":2.0}}}`)
	h := NewHub(Config{
		Spend:      func() json.RawMessage { return perDay },
		ClaudePlan: func() *ClaudePlan { return &ClaudePlan{WeeklyValueUSD: 70, ProjectedPctAtReset: 83} },
		SpendEvery: time.Minute,
	})
	h.pollSpend()
	h.pollClaudePlan()

	h.mu.Lock()
	charts := h.state.CostCharts
	h.mu.Unlock()
	if charts == nil || charts.Daily == nil || charts.Daily.Limit == nil {
		t.Fatal("the weekly limit line is missing after the plan poll")
	}
	if math.Abs(charts.Daily.Limit.Value-10) > 1e-9 {
		t.Errorf("limit = %v, want 10 a day", charts.Daily.Limit.Value)
	}
}
