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

// CostCharts is what the Cost panel draws its two charts from. The page draws
// only these values: every bar, the limit line and every label are decided
// here, so the unit tests can check them from fixture records.
type CostCharts struct {
	Daily  *CostChart `json:"daily,omitempty"`
	Hourly *CostChart `json:"hourly,omitempty"`
}

// CostChart is one bar chart: its bars in order, an optional horizontal line
// (the weekly limit), the number of malformed ledger lines skipped while reading
// it, and the aria-label that carries its numbers.
type CostChart struct {
	Bars    []CostBar `json:"bars"`
	Limit   *CostLine `json:"limit,omitempty"`
	Skipped int       `json:"skipped,omitempty"`
	Aria    string    `json:"aria"`
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
// block, nil when it has not reported. A missing input leaves its chart out
// rather than drawing zeros.
func BuildCostCharts(now time.Time, perDay map[string]float64, hours []SpendHour, skipped int, plan *ClaudePlan) CostCharts {
	return CostCharts{
		Daily:  buildDailyChart(now, perDay, plan),
		Hourly: buildHourlyChart(hours, skipped),
	}
}

// buildDailyChart draws the seven calendar days ending today, local time. A day
// with no figure is a gap, not a zero bar. A day with a top-up (a negative spend)
// is a zero bar, since a rise is money added, not spent. The weekly limit is its
// value spread over seven days, so it reads beside the bars in dollars, and its
// label says where the week's percentage lands at reset.
func buildDailyChart(now time.Time, perDay map[string]float64, plan *ClaudePlan) *CostChart {
	if len(perDay) == 0 {
		return nil
	}
	y, m, d := now.In(time.Local).Date()
	chart := &CostChart{Bars: make([]CostBar, 0, costDailyDays)}
	var words []string
	for i := costDailyDays - 1; i >= 0; i-- {
		day := time.Date(y, m, d-i, 0, 0, 0, 0, time.Local).Format("2006-01-02")
		spent, ok := perDay[day]
		label := day[len(day)-5:]
		if !ok {
			chart.Bars = append(chart.Bars, CostBar{Label: label, Gap: true, Title: day + " no figure"})
			continue
		}
		bar := CostBar{Label: label, Value: max(spent, 0), Title: day + " " + money(spent)}
		if spent < 0 {
			bar.Title = day + " +" + money(-spent) + " added"
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
	chart.Aria = aria
	return chart
}

// buildHourlyChart draws the day's spend by hour. An hour with no ledger sample
// is a gap, labeled, not a bar of zero. Malformed ledger lines the read skipped
// are named in the aria-label and counted on the chart.
func buildHourlyChart(hours []SpendHour, skipped int) *CostChart {
	if len(hours) == 0 {
		return nil
	}
	chart := &CostChart{Bars: make([]CostBar, 0, len(hours)), Skipped: skipped}
	var words []string
	gaps := 0
	for _, h := range hours {
		hr := h.Start.Format("15") + ":00"
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
