package dashboard

import (
	"math"
	"strings"
	"testing"
	"time"
)

// claudeSeriesNow ends every fixture window: the hourly chart's last bucket is
// 16:00 local and the daily chart's last day is 2026-10-25.
var claudeSeriesNow = time.Date(2026, 10, 25, 16, 30, 0, 0, time.Local)

// claudeSeriesDays is a spend report for the two days the daily fixtures read,
// so the daily chart is drawn and carries a Claude series.
var claudeSeriesDays = map[string]float64{"2026-10-24": 1.00, "2026-10-25": 2.00}

// claudeSeriesHours is a day of DeepSeek hours ending at claudeSeriesNow, every
// hour with a ledger sample; only the places the fixtures care about carry
// spend.
func claudeSeriesHours() []SpendHour {
	first := time.Date(2026, 10, 24, 17, 0, 0, 0, time.Local)
	hours := make([]SpendHour, spendHours)
	for i := range hours {
		hours[i] = SpendHour{Start: first.Add(time.Duration(i) * time.Hour), Samples: 1}
	}
	return hours
}

// claudeSeriesPlan is the default price: a week of the limit is worth
// 200 / 30.4 * 7 = $46.05, so one point of the weekly percentage is $0.4605.
var claudeSeriesPlan = &ClaudePlan{PlanUSDMonth: ClaudePlanDefaultUSD}

// claudeSeriesWeekUSD is the fixture's weekly value in dollars, which every
// expected figure below is a percentage of.
const claudeSeriesWeekUSD = 46.05

// claudeSeriesResetsAt is the fixture week's reset instant, later than every
// sample the fixtures write. Readings that name the same instant are one run of
// the limit, so the steps between them are spend and never a renewal.
var claudeSeriesResetsAt = time.Date(2026, 10, 29, 12, 0, 0, 0, time.Local)

// claudeSampleAt is one status line reading of the fixture week at a local
// instant, with the weekly percentage of the limit the fold prices.
func claudeSampleAt(at time.Time, sevenDay float64) ClaudePlanSample {
	return claudeSampleIn(claudeSeriesResetsAt, at, sevenDay)
}

// claudeSampleIn is a reading of the week that resets at resetsAt: a later
// reading naming another instant is the limit renewing between the two.
func claudeSampleIn(resetsAt, at time.Time, sevenDay float64) ClaudePlanSample {
	return ClaudePlanSample{T: at.Unix(), SevenDay: sevenDay, SevenDayResetsAt: resetsAt.Unix()}
}

// claudeSeries is the series as a poll that read it hands the charts.
func claudeSeries(samples ...ClaudePlanSample) PlanSamples {
	return PlanSamples{Samples: samples, Read: true}
}

// claudeBar finds one bucket of the series by its label.
func claudeBar(t *testing.T, s *CostClaudeSeries, label string) CostBar {
	t.Helper()
	for _, b := range s.Bars {
		if b.Label == label {
			return b
		}
	}
	t.Fatalf("the Claude series has no bucket %q", label)
	return CostBar{}
}

// Each step between two samples is worth its rise in the seven-day percentage of
// the week, counted in the bucket of the later sample; a bucket with a sample
// and no rise is a zero bar, and a bucket with no sample at all is a gap (gt-yxx98).
func TestBuildCostChartsPutsClaudeDollarsOnTheHourlyBuckets(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 10, 25, 15, 0, 0, 0, time.Local)
	samples := claudeSeries(
		claudeSampleAt(base, 10),
		claudeSampleAt(base.Add(20*time.Minute), 12),
		claudeSampleAt(base.Add(40*time.Minute), 13),
		claudeSampleAt(base.Add(time.Hour), 13), // 16:00: a sample, but no rise
	)
	c := BuildCostCharts(claudeSeriesNow, nil, claudeSeriesHours(), 0, claudeSeriesPlan, samples).Hourly
	if c == nil {
		t.Fatal("no hourly chart from 24 hours")
	}
	if c.Claude == nil {
		t.Fatalf("no Claude series: %s", c.ClaudeWhy)
	}
	if c.Claude.Name != claudePlanSeriesName {
		t.Errorf("series name = %q, want %q", c.Claude.Name, claudePlanSeriesName)
	}
	if len(c.Claude.Bars) != len(c.Bars) {
		t.Fatalf("Claude bars = %d, want one per chart bucket (%d)", len(c.Claude.Bars), len(c.Bars))
	}

	// Rises of 2 and 1 points: 3% of $46.05 = $1.38.
	if bar := claudeBar(t, c.Claude, "15:00"); math.Abs(bar.Value-1.38) > 1e-9 {
		t.Errorf("15:00 = %v, want 1.38 (3 points of the week)", bar.Value)
	} else if !strings.Contains(bar.Title, claudePlanSeriesName) {
		t.Errorf("15:00 tooltip %q does not say %q", bar.Title, claudePlanSeriesName)
	}
	// A reading that did not move is a zero, not a gap: the series did sample
	// this hour and saw nothing spent.
	if bar := claudeBar(t, c.Claude, "16:00"); bar.Gap || bar.Value != 0 {
		t.Errorf("16:00 = %+v, want a zero bar from a flat sample", bar)
	}
	// An hour the series has no sample for is a gap, never a zero.
	if bar := claudeBar(t, c.Claude, "03:00"); !bar.Gap {
		t.Errorf("03:00 = %+v, want a gap", bar)
	}
	if !strings.Contains(c.Aria, claudePlanSeriesName) {
		t.Errorf("aria %q does not name the Claude series", c.Aria)
	}
}

// A step is priced by the week its later reading belongs to, which the reading's
// own reset instant names: inside one week only a rise is spend, while a step
// that crossed a renewal is worth everything the new reading has spent since the
// limit rolled over (gt-yxx98).
func TestBuildCostChartsPricesAStepByItsWeek(t *testing.T) {
	t.Parallel()

	week, next := claudeSeriesResetsAt, claudeSeriesResetsAt.AddDate(0, 0, 7)
	at := func(h int) time.Time { return time.Date(2026, 10, 24, h, 0, 0, 0, time.Local) }
	for _, tc := range []struct {
		name    string
		samples PlanSamples
		want    float64
	}{
		{
			// 20 points of $46.05: ten before the renewal, the five the new week
			// had spent when the first reading of it arrived, and five more by
			// the last. The drop from 90 to 5 is the limit starting over, never
			// money returned, so it costs the day nothing.
			name: "a renewal between two readings charges the new week's spend and not the drop",
			samples: claudeSeries(
				claudeSampleIn(week, at(10), 80),
				claudeSampleIn(week, at(12), 90),
				claudeSampleIn(next, at(14), 5),
				claudeSampleIn(next, at(16), 10),
			),
			want: 9.21,
		},
		{
			// The new week rose past the old week's figure before the next
			// sample: it spent all 70 of its points, not the 10-point difference
			// a fall is mistaken for.
			name:    "a renewal past the old week's figure charges the new week's whole spend",
			samples: claudeSeries(claudeSampleIn(week, at(10), 60), claudeSampleIn(next, at(12), 70)),
			want:    32.24,
		},
		{
			// A fall inside one week is the API's own correction, not a renewal.
			name:    "a fall inside one week is not spend",
			samples: claudeSeries(claudeSampleIn(week, at(10), 60), claudeSampleIn(week, at(12), 50)),
			want:    0,
		},
		{
			// With no reset instant a renewal cannot be told from a fall, so the
			// reading is read as the fall it might be.
			name:    "a fall with no reset instant is not a renewal",
			samples: claudeSeries(ClaudePlanSample{T: at(10).Unix(), SevenDay: 60}, ClaudePlanSample{T: at(12).Unix(), SevenDay: 50}),
			want:    0,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := BuildCostCharts(claudeSeriesNow, claudeSeriesDays, nil, 0, claudeSeriesPlan, tc.samples).Daily
			if c == nil || c.Claude == nil {
				t.Fatalf("no Claude series on the daily chart: %+v", c)
			}
			if bar := claudeBar(t, c.Claude, "10-24"); math.Abs(bar.Value-tc.want) > 1e-9 {
				t.Errorf("10-24 = %v, want %v", bar.Value, tc.want)
			}
		})
	}
}

// A day the series has no sample for is a gap, so the page hatches it instead of
// drawing a bar of no spend (gt-yxx98).
func TestBuildCostChartsHatchesTheDaysWithNoClaudeSample(t *testing.T) {
	t.Parallel()

	day := func(h int) time.Time { return time.Date(2026, 10, 25, h, 0, 0, 0, time.Local) }
	samples := claudeSeries(
		claudeSampleAt(day(9), 40),
		claudeSampleAt(day(10), 41), // +1 point
	)
	c := BuildCostCharts(claudeSeriesNow, claudeSeriesDays, nil, 0, claudeSeriesPlan, samples).Daily
	if c == nil || c.Claude == nil {
		t.Fatal("no Claude series on the daily chart")
	}
	if bar := claudeBar(t, c.Claude, "10-25"); bar.Gap || math.Abs(bar.Value-0.46) > 1e-9 {
		t.Errorf("10-25 = %+v, want 0.46 (one point of $46.05)", bar)
	}
	for _, label := range []string{"10-19", "10-24"} {
		bar := claudeBar(t, c.Claude, label)
		if !bar.Gap || bar.Value != 0 {
			t.Errorf("%s = %+v, want a gap", label, bar)
		}
		if !strings.Contains(bar.Title, "no sample") {
			t.Errorf("%s tooltip %q does not say it has no sample", label, bar.Title)
		}
	}
	if !strings.Contains(c.Aria, "6 buckets have no sample") {
		t.Errorf("aria %q does not count the buckets with no sample", c.Aria)
	}
}

// A series no poll has read, one that could not be read, one the status line has
// not written yet, and one with no sample in the window draw no Claude series
// and each say which they are: a row of zeros would read as a week of no plan
// spend, and reading a file that was never opened as an empty status line blames
// the wrong thing (gt-yxx98).
func TestBuildCostChartsSaysWhyItHasNoClaudeSeries(t *testing.T) {
	t.Parallel()

	stale := claudeSampleAt(time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local), 40)
	for _, tc := range []struct {
		name    string
		samples PlanSamples
		want    string
	}{
		{"not read", PlanSamples{}, "not been read"},
		{"empty", PlanSamples{Read: true}, "no sample"},
		{"unreadable", PlanSamples{Read: true, Failed: true}, "could not be read"},
		{"none in the window", claudeSeries(stale), "in this window"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := BuildCostCharts(claudeSeriesNow, claudeSeriesDays, claudeSeriesHours(), 0, claudeSeriesPlan, tc.samples)
			if c.Daily == nil || c.Hourly == nil {
				t.Fatal("the DeepSeek charts are not both drawn")
			}
			for name, chart := range map[string]*CostChart{"daily": c.Daily, "hourly": c.Hourly} {
				if chart.Claude != nil {
					t.Errorf("the %s chart drew %d Claude bars for %s", name, len(chart.Claude.Bars), tc.name)
				}
				if !strings.Contains(chart.ClaudeWhy, tc.want) {
					t.Errorf("the %s chart's reason %q does not say %q", name, chart.ClaudeWhy, tc.want)
				}
			}
		})
	}
}

// The week's dollars follow the plan's price, so the same sample series prices
// higher on a dearer plan; a plan that has not reported prices the default.
func TestClaudeWeekUSDFollowsThePlanPrice(t *testing.T) {
	t.Parallel()

	if got, want := claudeWeekUSD(claudeSeriesPlan), claudeSeriesWeekUSD; math.Abs(got-want) > 1e-9 {
		t.Errorf("the default plan's week = %v, want %v", got, want)
	}
	if got := claudeWeekUSD(nil); math.Abs(got-claudeSeriesWeekUSD) > 1e-9 {
		t.Errorf("an absent plan's week = %v, want the default %v", got, claudeSeriesWeekUSD)
	}
	if got, want := claudeWeekUSD(&ClaudePlan{PlanUSDMonth: 100}), 23.03; math.Abs(got-want) > 1e-9 {
		t.Errorf("a $100 plan's week = %v, want %v", got, want)
	}
}
