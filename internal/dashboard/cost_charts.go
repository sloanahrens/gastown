package dashboard

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// costDailyDays is how many days the daily spend chart draws: the seven calendar
// days ending today, each a bar or a gap.
const costDailyDays = 7

// CostCharts is what the Cost panel draws its two charts and its headline's
// Claude figures from. The page draws only these values: every bar, the limit
// line, every label and every dollar are decided here, so the unit tests can
// check them from fixture records.
type CostCharts struct {
	Headline *CostHeadline `json:"headline,omitempty"`
	Daily    *CostChart    `json:"daily,omitempty"`
	Hourly   *CostChart    `json:"hourly,omitempty"`
}

// CostChart is one bar chart: its bars in order, the Claude series on the same
// buckets, an optional horizontal line (the weekly limit), the number of
// malformed ledger lines skipped while reading it, and the aria-label that
// carries its numbers.
type CostChart struct {
	Bars   []CostBar         `json:"bars"`
	Claude *CostClaudeSeries `json:"claude,omitempty"`
	// ClaudeWhy is why the chart carries no Claude series: the samples could not
	// be read, there are none, or none falls in the window.
	ClaudeWhy string    `json:"claude_why,omitempty"`
	Limit     *CostLine `json:"limit,omitempty"`
	Skipped   int       `json:"skipped,omitempty"`
	Aria      string    `json:"aria"`
}

// CostClaudeSeries is the Claude plan's plan-equivalent dollars on the chart's
// own buckets, drawn in a second color: one bar or gap per bucket, so the
// operator reads both spends against one axis.
type CostClaudeSeries struct {
	// Name is the legend entry and the label every tooltip ends with; it says
	// the dollars are an estimate of the plan's share, not a metered bill.
	Name string `json:"name"`
	// Bars run in the same order as the chart's bars: a value, or a gap the page
	// hatches because the series has no sample for that bucket.
	Bars []CostBar `json:"bars"`
}

// CostBar is one bar: its value in dollars, the label under it, the hover
// text, and whether it is a gap (no data) rather than a value of zero.
type CostBar struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Title string  `json:"title"`
	Gap   bool    `json:"gap,omitempty"`
}

// CostLine is the horizontal line across a chart: its value in dollars and its
// label.
type CostLine struct {
	Value float64 `json:"value"`
	Label string  `json:"label"`
}

// BuildCostCharts builds the Cost panel's charts. now is the clock the days and
// hours end at; perDay is the spend report's per-day spend, nil when there is no
// report; hours is the day's spend by hour from the balance ledger, and skipped
// is how many malformed ledger lines the read skipped; plan is the Claude plan
// block, nil when it has not reported, and samples is the status line's series
// the Claude dollars are folded from. A missing input leaves its chart out, or
// its series out with the reason on the chart, rather than drawing zeros.
func BuildCostCharts(now time.Time, perDay map[string]float64, hours []SpendHour, skipped int, plan *ClaudePlan, samples PlanSamples) CostCharts {
	weeklyUSD := claudeWeekUSD(plan)
	return CostCharts{
		Headline: claudeHeadline(now, samples, weeklyUSD),
		Daily:    buildDailyChart(now, perDay, plan, samples, weeklyUSD),
		Hourly:   buildHourlyChart(hours, skipped, samples, weeklyUSD),
	}
}

// costDailyBuckets is the daily chart's buckets: the seven local calendar days
// ending on now's day.
func costDailyBuckets(now time.Time) []costBucket {
	y, m, d := now.In(time.Local).Date()
	buckets := make([]costBucket, 0, costDailyDays)
	for i := costDailyDays - 1; i >= 0; i-- {
		start := time.Date(y, m, d-i, 0, 0, 0, 0, time.Local)
		day := start.Format("2006-01-02")
		buckets = append(buckets, costBucket{Label: day[len(day)-5:], Name: day, Start: start})
	}
	return buckets
}

// buildDailyChart draws the seven calendar days ending today, local time. A day
// with no figure is a gap, not a zero bar. A day with a top-up (a negative spend)
// is a zero bar, since a rise is money added, not spent. The weekly limit is its
// value spread over seven days, so it reads beside the bars in dollars, and its
// label says where the week's percentage lands at reset.
func buildDailyChart(now time.Time, perDay map[string]float64, plan *ClaudePlan, samples PlanSamples, weeklyUSD float64) *CostChart {
	if len(perDay) == 0 {
		return nil
	}
	chart := &CostChart{Bars: make([]CostBar, 0, costDailyDays)}
	buckets := costDailyBuckets(now)
	var words []string
	for _, b := range buckets {
		spent, ok := perDay[b.Name]
		if !ok {
			chart.Bars = append(chart.Bars, CostBar{Label: b.Label, Gap: true, Title: b.Name + " no figure"})
			continue
		}
		bar := CostBar{Label: b.Label, Value: max(spent, 0), Title: b.Name + " " + money(spent)}
		if spent < 0 {
			bar.Title = b.Name + " +" + money(-spent) + " added"
		}
		chart.Bars = append(chart.Bars, bar)
		words = append(words, bar.Title)
	}
	aria := fmt.Sprintf("Daily spend, last %d days: ", costDailyDays)
	if len(words) > 0 {
		aria += strings.Join(words, ", ")
	} else {
		aria += "no figures"
	}
	if plan != nil && plan.WeeklyValueUSD > 0 {
		daily := plan.WeeklyValueUSD / costDailyDays
		pct := pctText(plan.ProjectedPctAtReset)
		chart.Limit = &CostLine{Value: daily, Label: "weekly limit " + money(daily) + "/day · reset at " + pct}
		aria += ". Weekly limit " + money(daily) + " a day. Projected at reset " + pct + " of the weekly limit"
	}
	lastEnd := buckets[len(buckets)-1].Start.AddDate(0, 0, 1)
	chart.Claude, chart.ClaudeWhy = claudeCostSeries(buckets, lastEnd, samples, weeklyUSD)
	if chart.Claude != nil {
		aria += claudeSeriesAria(chart.Claude)
	}
	chart.Aria = aria
	return chart
}

// buildHourlyChart draws the day's spend by hour. An hour with no ledger sample
// is a gap, labeled, not a bar of zero. Malformed ledger lines the read skipped
// are named in the aria-label and counted on the chart.
func buildHourlyChart(hours []SpendHour, skipped int, samples PlanSamples, weeklyUSD float64) *CostChart {
	if len(hours) == 0 {
		return nil
	}
	chart := &CostChart{Bars: make([]CostBar, 0, len(hours)), Skipped: skipped}
	buckets := make([]costBucket, len(hours))
	var words []string
	gaps := 0
	for i, h := range hours {
		hr := h.Start.Format("15") + ":00"
		buckets[i] = costBucket{Label: hr, Name: hr, Start: h.Start}
		if h.Samples == 0 {
			gaps++
			chart.Bars = append(chart.Bars, CostBar{Label: hr, Gap: true, Title: hr + " no ledger sample"})
			continue
		}
		bar := CostBar{Label: hr, Value: max(h.Spent, 0), Title: hr + " " + money(h.Spent)}
		chart.Bars = append(chart.Bars, bar)
		if h.Spent > 0 {
			words = append(words, bar.Title)
		}
	}
	aria := fmt.Sprintf("Hourly spend, last %d hours", len(hours))
	if len(words) > 0 {
		aria += ": " + strings.Join(words, ", ")
	} else {
		aria += ": no spend"
	}
	if gaps > 0 {
		aria += fmt.Sprintf(". %d %s no ledger sample", gaps, plural(gaps, "hour has", "hours have"))
	}
	if skipped > 0 {
		aria += fmt.Sprintf(". %d malformed ledger %s skipped", skipped, plural(skipped, "line", "lines"))
	}
	lastEnd := hours[len(hours)-1].Start.Add(time.Hour)
	chart.Claude, chart.ClaudeWhy = claudeCostSeries(buckets, lastEnd, samples, weeklyUSD)
	if chart.Claude != nil {
		aria += claudeSeriesAria(chart.Claude)
	}
	chart.Aria = aria
	return chart
}

// SpendPerDay reads the per-day spend out of the spend report's JSON, nil when
// the report is absent or not the shape the page reads.
func SpendPerDay(raw json.RawMessage) map[string]float64 {
	if len(raw) == 0 {
		return nil
	}
	var report struct {
		PerDay map[string]struct {
			Spent *float64 `json:"spent"`
		} `json:"per_day"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil
	}
	out := make(map[string]float64, len(report.PerDay))
	for day, v := range report.PerDay {
		if v.Spent != nil {
			out[day] = *v.Spent
		}
	}
	return out
}

// pctText is a percentage as a whole number, rounded half away from zero, so a
// negative projection rounds the same way a positive one does.
func pctText(p float64) string {
	return fmt.Sprintf("%d%%", int(math.Round(p)))
}

// plural is one or many, by n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// money formats a dollar figure as the page does.
func money(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}
