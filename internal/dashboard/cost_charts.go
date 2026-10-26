package dashboard

import (
	"encoding/json"
	"fmt"
	"sort"
)

// costDailyDays is how many days the daily spend chart draws.
const costDailyDays = 7

// CostCharts is what the Cost panel draws its two charts from. The page draws
// only these values: every bar, the limit line and every label are decided
// here, so the unit tests can check them from fixture records.
type CostCharts struct {
	Daily  *CostChart `json:"daily,omitempty"`
	Hourly *CostChart `json:"hourly,omitempty"`
}

// CostChart is one bar chart: its bars in order, an optional horizontal line
// (the weekly limit), and the aria-label that carries its numbers.
type CostChart struct {
	Bars  []CostBar `json:"bars"`
	Limit *CostLine `json:"limit,omitempty"`
	Aria  string    `json:"aria"`
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

// BuildCostCharts builds the Cost panel's charts. perDay is the spend report's
// per-day spend, nil when there is no report; hours is the day's spend by hour
// from the balance ledger; plan is the Claude plan block, nil when it has not
// reported. A missing input leaves its chart out rather than drawing zeros.
func BuildCostCharts(perDay map[string]float64, hours []SpendHour, plan *ClaudePlan) CostCharts {
	return CostCharts{
		Daily:  buildDailyChart(perDay, plan),
		Hourly: buildHourlyChart(hours),
	}
}

// buildDailyChart draws the last seven days of spend. A day with a top-up (a
// negative spend) is a zero bar, since a rise is money added, not spent. The
// weekly limit is its value spread over seven days, so it reads beside the bars
// in dollars, and its label says where the week's percentage lands at reset.
func buildDailyChart(perDay map[string]float64, plan *ClaudePlan) *CostChart {
	if len(perDay) == 0 {
		return nil
	}
	days := make([]string, 0, len(perDay))
	for day := range perDay {
		days = append(days, day)
	}
	sort.Strings(days)
	if len(days) > costDailyDays {
		days = days[len(days)-costDailyDays:]
	}
	chart := &CostChart{Bars: make([]CostBar, 0, len(days))}
	var words []string
	for _, day := range days {
		spent := perDay[day]
		label := day
		if len(day) >= 5 {
			label = day[len(day)-5:]
		}
		bar := CostBar{Label: label, Value: max(spent, 0), Title: day + " " + money(spent)}
		if spent < 0 {
			bar.Title = day + " +" + money(-spent) + " added"
		}
		chart.Bars = append(chart.Bars, bar)
		words = append(words, bar.Title)
	}
	aria := fmt.Sprintf("Daily spend, last %d days: ", len(days)) + joinWords(words)
	if plan != nil && plan.WeeklyValueUSD > 0 {
		daily := plan.WeeklyValueUSD / costDailyDays
		label := "weekly limit " + money(daily) + "/day"
		aria += ". Weekly limit " + money(daily) + " a day"
		// A projection of exactly 0% is a real figure, so the label always names it.
		label += fmt.Sprintf(" · reset at %d%%", int(plan.ProjectedPctAtReset+0.5))
		aria += fmt.Sprintf(". Projected at reset %d%% of the weekly limit", int(plan.ProjectedPctAtReset+0.5))
		chart.Limit = &CostLine{Value: daily, Label: label}
	}
	chart.Aria = aria
	return chart
}

// buildHourlyChart draws the day's spend by hour. An hour with no ledger sample
// is a gap, labeled, not a bar of zero.
func buildHourlyChart(hours []SpendHour) *CostChart {
	if len(hours) == 0 {
		return nil
	}
	chart := &CostChart{Bars: make([]CostBar, 0, len(hours))}
	var words []string
	gaps := 0
	for _, h := range hours {
		hr := h.Start.Local().Format("15") + ":00"
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
		aria += ": " + joinWords(words)
	} else {
		aria += ": no spend"
	}
	if gaps > 0 {
		if gaps == 1 {
			aria += ". 1 hour has no ledger sample"
		} else {
			aria += fmt.Sprintf(". %d hours have no ledger sample", gaps)
		}
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

// joinWords joins the parts with the separator the panel uses between figures.
func joinWords(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// money formats a dollar figure as the page does.
func money(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}
