package dashboard

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CostProvider is one row of the Cost panel's providers table. Every provider
// carries the same fields, so the page draws one row shape and adding a
// provider is one more builder here. A nil figure is one the provider does not
// have or could not read, never a zero.
type CostProvider struct {
	ID    string `json:"id"`    // "deepseek", "claude", "jev"
	Label string `json:"label"` // the row's name on the page
	Kind  string `json:"kind"`  // "cash" or "plan"
	// Balance is the dollars left, cash kind.
	Balance *float64 `json:"balance"`
	// PlanPct is the percent of the weekly limit used, plan kind.
	PlanPct    *float64 `json:"plan_pct"`
	Today      *float64 `json:"today"`
	Yesterday  *float64 `json:"yesterday"`
	Avg7d      *float64 `json:"avg_7d"`
	PerHour1h  *float64 `json:"per_hour_1h"`
	PerHour24h *float64 `json:"per_hour_24h"`
	Runway     *float64 `json:"runway_days"`
	// Status is "ok", "warn", "bad" or "none" when there is nothing to judge.
	Status     string `json:"status"`
	StatusText string `json:"status_text"`
	// Detail is the hover lines: cash and credit, recent top-ups, ledger time.
	Detail []string `json:"detail,omitempty"`
	// Warnings are lines that must stay visible whatever the figures say.
	Warnings []string `json:"warnings,omitempty"`
	// Why is set when the provider could not be read; its figures are nil.
	Why string `json:"why,omitempty"`
}

// Provider IDs, in the order the panel draws them.
const (
	costProviderDeepSeek = "deepseek"
	costProviderClaude   = "claude"
	costProviderJev      = "jev"
)

// costProviderBuilder builds one provider's row from the inputs the Cost panel
// reads.
type costProviderBuilder func(now time.Time, spend json.RawMessage, plan *ClaudePlan, charts *CostCharts, jev *JevSpend) CostProvider

// costProviderBuilders is the providers' rows in the order the panel draws
// them. A new provider is one more entry.
var costProviderBuilders = []costProviderBuilder{
	deepSeekCostProvider,
	claudeCostProvider,
	jevCostProvider,
}

// BuildCostProviders builds every provider's row from the same inputs the Cost
// charts are built from, in costProviderBuilders' order.
func BuildCostProviders(now time.Time, spend json.RawMessage, plan *ClaudePlan, charts *CostCharts, jev *JevSpend) []CostProvider {
	return buildCostProviders(costProviderBuilders, now, spend, plan, charts, jev)
}

// buildCostProviders runs the given builders over one set of inputs, in order.
func buildCostProviders(builders []costProviderBuilder, now time.Time, spend json.RawMessage, plan *ClaudePlan, charts *CostCharts, jev *JevSpend) []CostProvider {
	out := make([]CostProvider, 0, len(builders))
	for _, build := range builders {
		out = append(out, build(now, spend, plan, charts, jev))
	}
	return out
}

// deepSeekSpend is the fields of the spend report the DeepSeek row reads.
type deepSeekSpend struct {
	Balance      *float64         `json:"balance"`
	Pace1h       deepSeekPace     `json:"pace_1h"`
	Pace24h      deepSeekPace     `json:"pace_24h"`
	RunwayDays   *float64         `json:"runway_days_at_24h_pace"`
	Cash         *float64         `json:"cash"`
	Credit       *float64         `json:"credit"`
	CreditRunway *float64         `json:"credit_runway_days_at_24h_pace"`
	Credits      []deepSeekCredit `json:"credits"`
	PerDay       map[string]struct {
		Spent *float64 `json:"spent"`
	} `json:"per_day"`
	Live struct {
		// OK is the report's own "the live read succeeded" and Available its
		// "the balance came from it"; writers in the field have sent either,
		// and the page colors the balance from Available alone.
		OK        *bool `json:"ok"`
		Available *bool `json:"available"`
	} `json:"live"`
	// WatcherPID is null or empty when the spend watcher is not running.
	WatcherPID any    `json:"watcher_pid"`
	LastSample string `json:"last_sample"`
}

// deepSeekPace is a spend window's rate, in dollars an hour.
type deepSeekPace struct {
	PerHour *float64 `json:"per_hour"`
}

// deepSeekCredit is one balance rise the report lists.
type deepSeekCredit struct {
	At     string  `json:"at"`
	Amount float64 `json:"amount"`
	Kind   string  `json:"kind"`
}

// deepSeekCostProvider is the DeepSeek row: the cash balance, its spend rates,
// the days the report's per-day spend reads, and the runway.
func deepSeekCostProvider(_ time.Time, spend json.RawMessage, _ *ClaudePlan, _ *CostCharts, _ *JevSpend) CostProvider {
	p := CostProvider{ID: costProviderDeepSeek, Label: "DeepSeek", Kind: "cash", Status: "none"}
	var d *deepSeekSpend
	if len(spend) == 0 || json.Unmarshal(spend, &d) != nil || d == nil {
		p.Why = "the spend report could not be read"
		return p
	}
	p.Balance = d.Balance
	p.PerHour1h = d.Pace1h.PerHour
	p.PerHour24h = d.Pace24h.PerHour
	p.Runway = d.RunwayDays
	if d.RunwayDays != nil {
		p.StatusText = fmt.Sprintf("%.1fd runway", *d.RunwayDays)
		p.Status = runwayStatus(*d.RunwayDays)
	}

	if d.Cash != nil && d.Credit != nil {
		line := "DeepSeek " + money(*d.Cash) + " cash + " + money(*d.Credit) + " credit"
		if *d.Credit > 0 && d.CreditRunway != nil {
			line += fmt.Sprintf(" · credit lasts ~%.1fd, then cash", *d.CreditRunway)
		}
		p.Detail = append(p.Detail, line)
	}

	days := make([]string, 0, len(d.PerDay))
	for day := range d.PerDay {
		days = append(days, day)
	}
	sort.Strings(days)
	spentOn := func(i int) *float64 {
		if i < 0 {
			return nil
		}
		return d.PerDay[days[i]].Spent
	}
	// A negative day is a balance rise: no spend for that day, and the rise
	// goes to the hover so it is still visible.
	dayFigure := func(i int) *float64 {
		v := spentOn(i)
		if v == nil {
			return nil
		}
		if *v < 0 {
			p.Detail = append(p.Detail, days[i]+" +"+money(-*v)+" added")
			return nil
		}
		f := *v
		return &f
	}
	n := len(days)
	p.Today = dayFigure(n - 1)
	p.Yesterday = dayFigure(n - 2)

	// The mean is the rate of the complete days before today: a day with no
	// figure is left out, and so is a rise, which says what was added, not
	// what the day cost.
	var complete []float64
	for i := 0; i < n-1; i++ {
		if v := spentOn(i); v != nil && *v >= 0 {
			complete = append(complete, *v)
		}
	}
	if len(complete) > costDailyDays {
		complete = complete[len(complete)-costDailyDays:]
	}
	if len(complete) > 0 {
		var sum float64
		for _, v := range complete {
			sum += v
		}
		avg := sum / float64(len(complete))
		p.Avg7d = &avg
	}

	// Credits come oldest first, so the two newest are read from the end.
	for i := len(d.Credits) - 1; i >= 0 && i >= len(d.Credits)-2; i-- {
		p.Detail = append(p.Detail, creditRiseText(d.Credits[i]))
	}
	if d.LastSample != "" {
		p.Detail = append(p.Detail, "ledger "+strings.Replace(d.LastSample, "T", " ", 1))
	}

	if liveUnreachable(d.Live.OK, d.Live.Available) {
		p.Warnings = append(p.Warnings, "live balance unreachable")
	}
	if d.WatcherPID == nil || d.WatcherPID == "" {
		p.Warnings = append(p.Warnings, "spend watcher is not running")
	}
	return p
}

// liveUnreachable is whether the report leaves the balance on its ledger rather
// than a live read. It fails closed: a writer that sent neither field, or that
// reported the read failed through either one, has not said the balance is
// live, and the page colors the figure bad for Available as well as OK. A
// field that does confirm the read keeps the warning away.
func liveUnreachable(ok, available *bool) bool {
	if ok == nil && available == nil {
		return true
	}
	return (ok != nil && !*ok) || (available != nil && !*available)
}

// runwayStatus is the color of a runway in days: bad under three, warn under
// seven, ok otherwise.
func runwayStatus(days float64) string {
	switch {
	case days < 3:
		return "bad"
	case days < 7:
		return "warn"
	}
	return "ok"
}

// creditRiseText is one balance rise as the Cost panel's hover words it.
func creditRiseText(c deepSeekCredit) string {
	var kind string
	switch c.Kind {
	case "credit":
		kind = " credit"
	case "top-up":
		kind = " top-up"
	case "credit+top-up":
		kind = " credit + top-up"
	default:
		kind = " added"
	}
	// The stamp reads month, day and time: the year and the seconds are noise
	// beside the rise itself.
	at := c.At
	if len(at) >= 16 {
		at = at[5:16]
	}
	return strings.Replace(at, "T", " ", 1) + " +" + money(c.Amount) + kind
}

// claudeCostProvider is the Claude plan row. Its dollars are the plan's share of
// the month, and its today and 24-hour rate are the headline's, so the table
// and the charts agree.
func claudeCostProvider(now time.Time, _ json.RawMessage, plan *ClaudePlan, charts *CostCharts, _ *JevSpend) CostProvider {
	p := CostProvider{ID: costProviderClaude, Label: "Claude", Kind: "plan", Status: "none"}
	if plan == nil {
		p.Why = "the Claude plan has not been read yet"
		return p
	}
	if plan.Unavailable || plan.Stale {
		p.Why = claudePlanUnreadWhy(plan, now)
		return p
	}
	pct := plan.WeeklyPct
	p.PlanPct = &pct

	if charts != nil && charts.Headline != nil {
		p.Today = copyFloat(charts.Headline.Today)
		p.PerHour24h = copyFloat(charts.Headline.PerHour24h)
	}
	var bars []CostBar
	if charts != nil && charts.Daily != nil && charts.Daily.Claude != nil {
		bars = charts.Daily.Claude.Bars
	}
	if n := len(bars); n > 0 {
		p.Yesterday = barFigure(bars, n-2)
		// The mean is of the complete days before today, capped at the chart's
		// week the way the DeepSeek row caps its own: a longer series must not
		// turn the seven-day average into a month's.
		var complete []float64
		for _, b := range bars[:n-1] {
			if !b.Gap {
				complete = append(complete, b.Value)
			}
		}
		if len(complete) > costDailyDays {
			complete = complete[len(complete)-costDailyDays:]
		}
		if len(complete) > 0 {
			var sum float64
			for _, v := range complete {
				sum += v
			}
			avg := sum / float64(len(complete))
			p.Avg7d = &avg
		}
	}

	// The plan's pace as dollars: percentage points a day, a day's 24 parts,
	// a point of the weekly limit's value.
	perHour := func(pacePerDay float64) float64 {
		return pacePerDay / 24 / claudePlanLimitPct * plan.WeeklyValueUSD
	}
	h1 := perHour(plan.PacePerDay1h)
	p.PerHour1h = &h1
	p.Detail = append(p.Detail, claudeFiveHourLine(plan), "plan pace last 24h "+money(perHour(plan.PacePerDay24h))+"/h, approximate")
	if charts != nil && charts.Headline != nil {
		if h := charts.Headline; h.HoursRead > 0 && h.HoursRead < h.WindowHours {
			p.Detail = append(p.Detail, fmt.Sprintf("%d of %d hours read", h.HoursRead, h.WindowHours))
		}
	}
	p.Detail = append(p.Detail, claudeHoverLines(plan)...)

	p.Status = planStatus(plan.WeeklyPct, plan.ProjectedPctAtReset)
	if h := plan.HitsLimitInDays; h != nil {
		if *h > 0 {
			p.StatusText = fmt.Sprintf("limit in %.1fd", *h)
		} else {
			p.StatusText = "at the limit"
		}
	}
	return p
}

// claudeFiveHourLine is the five-hour limit in use and when it rolls over; a
// sample that carried no reset says so rather than drawing a time.
func claudeFiveHourLine(plan *ClaudePlan) string {
	line := "5-hour limit " + claudePct(plan.FiveHourPct) + " used"
	if plan.FiveHourResetsAt == 0 {
		return line + ", no reset instant in the newest sample"
	}
	return line + ", resets " + time.Unix(plan.FiveHourResetsAt, 0).Format("Mon 15:04")
}

// claudeHoverLines are the plan's hover lines beyond the row's figures: the
// API-equivalent dollars when the plan has both, the billing month's use and
// weeks at the ceiling when a week is counted, and the state the poll could not
// keep. The dollars are approximate: plans weight tokens differently from the
// API, and an unpriced model counts as zero, so the figure is then a floor.
func claudeHoverLines(plan *ClaudePlan) []string {
	var lines []string
	if w, m := plan.APIEquivalentWeekUSD, plan.APIEquivalentMonthUSD; w != nil && m != nil {
		line := "API-equivalent " + money(*w) + " this week, " + money(*m) + " this month, approximate: plans weight tokens differently from the API"
		if len(plan.Unpriced) > 0 {
			line += " · counted at zero, no list price: " + strings.Join(plan.Unpriced, ", ")
		}
		lines = append(lines, line)
	}
	if plan.WeeksCounted > 0 {
		line := "month " + claudePct(plan.UtilizationPct) + " of plan"
		if n := plan.WeeksAtCeiling; n > 0 {
			line += fmt.Sprintf(" · %d %s at ceiling", n, plural(n, "week", "weeks"))
		}
		lines = append(lines, line)
	}
	if plan.StateError != "" {
		lines = append(lines, "state not kept: "+plan.StateError)
	}
	return lines
}

// claudePct reads a percentage of a limit whole, as the status line samples it.
func claudePct(v float64) string {
	return fmt.Sprintf("%.0f%%", math.Round(v))
}

// planStatus is the row's color: a week already at the ceiling the month rollup
// counts, or a projection past the limit, is bad; a projection past the ceiling
// warns; anything else is ok. The page draws the chip from this status alone, so
// the rule lives here and nowhere else.
func planStatus(weeklyPct, projectedPct float64) string {
	switch {
	case weeklyPct >= ClaudePlanCeilingPct || projectedPct > claudePlanLimitPct:
		return "bad"
	case projectedPct > ClaudePlanCeilingPct:
		return "warn"
	}
	return "ok"
}

// claudePlanUnreadWhy is why the plan has no figures, in the page's words.
func claudePlanUnreadWhy(plan *ClaudePlan, now time.Time) string {
	switch {
	case plan.Unavailable:
		return "the status line's samples could not be read"
	case plan.SampledAt == 0:
		return "the status line has written no samples yet"
	case plan.WeeklyResetsAt != 0 && !time.Unix(plan.WeeklyResetsAt, 0).After(now):
		return "the week the newest sample measures has already reset"
	}
	return "the newest sample is " + costDur(now.Sub(time.Unix(plan.SampledAt, 0)).Seconds()) + " old"
}

// jevCostProvider is the Jev row: the window's calls and dollars, with a rate
// only when the window is a day. A read that is not current leaves the figures
// off the row, where they would read as today's, and puts them in its hover.
func jevCostProvider(_ time.Time, _ json.RawMessage, _ *ClaudePlan, _ *CostCharts, jev *JevSpend) CostProvider {
	p := CostProvider{ID: costProviderJev, Label: "Jev", Kind: "cash", Status: "none"}
	if jev == nil {
		p.Why = "the Jev spend could not be read"
		return p
	}
	if jev.Stale {
		p.Why = jevUnreadWhy(jev)
		p.Detail = append(p.Detail, jevLastGoodMoney(jev))
		p.Detail = append(p.Detail, jevCallLines(jev)...)
		return p
	}
	if hours, ok := windowHours(jev.Window); ok && hours == 24 {
		rate := jev.USD / 24
		p.PerHour24h = &rate
	}
	p.Detail = append(p.Detail, jevCallLines(jev)...)
	return p
}

// jevUnreadWhy is why the row draws no figures, in the page's words: a failed
// read says so, and one that only aged says it went stale.
func jevUnreadWhy(jev *JevSpend) string {
	if jev.ReadFailed {
		return "the read failed"
	}
	return "the read went stale"
}

// jevLastGoodMoney is a stale row's money in its hover: the window's dollars,
// and the hourly rate its own cell would carry when the window is a day.
func jevLastGoodMoney(jev *JevSpend) string {
	line := "last good: " + money(jev.USD)
	if jev.Window != "" {
		line += " over " + jev.Window
	}
	if hours, ok := windowHours(jev.Window); ok && hours == 24 {
		line += " · " + money(jev.USD/24) + "/h"
	}
	return line
}

// jevCallLines is the row's hover lines for the window's calls and the caller
// that cost the most.
func jevCallLines(jev *JevSpend) []string {
	calls := fmt.Sprintf("%d %s", jev.Calls, plural(jev.Calls, "call", "calls"))
	if jev.Window != "" {
		calls += " in " + jev.Window
	}
	lines := []string{calls}
	if jev.TopCaller != "" {
		lines = append(lines, fmt.Sprintf("top caller %s (%d %s, %s)", jev.TopCaller, jev.TopCallerCalls, plural(jev.TopCallerCalls, "call", "calls"), money(jev.TopCallerUSD)))
	}
	return lines
}

// windowHours is a spend window's length in hours. The reader writes it as the
// Go duration form ("24h"), and a days form ("1d") or a bare number of hours is
// read too, so a second writer's spelling does not silently drop the rate.
func windowHours(window string) (float64, bool) {
	if window == "" {
		return 0, false
	}
	if w, err := time.ParseDuration(window); err == nil {
		return w.Hours(), true
	}
	if days, ok := strings.CutSuffix(window, "d"); ok {
		if n, err := strconv.ParseFloat(days, 64); err == nil && n > 0 {
			return n * 24, true
		}
		return 0, false
	}
	if n, err := strconv.ParseFloat(window, 64); err == nil && n > 0 {
		return n, true
	}
	return 0, false
}

// barFigure is the value of bar i as a figure, nil when the bucket is a gap.
func barFigure(bars []CostBar, i int) *float64 {
	if i < 0 || bars[i].Gap {
		return nil
	}
	v := bars[i].Value
	return &v
}

// copyFloat copies a figure so a row does not share its source's storage.
func copyFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// costDur is a span in the page's words: seconds, minutes, hours and minutes,
// or days.
func costDur(secs float64) string {
	if secs < 0 {
		return "–"
	}
	s := int(secs + 0.5)
	if s < 90 {
		return fmt.Sprintf("%ds", s)
	}
	m := (s + 30) / 60
	if m < 90 {
		return fmt.Sprintf("%dm", m)
	}
	h := m / 60
	if h >= 48 {
		return fmt.Sprintf("%dd", h/24)
	}
	return fmt.Sprintf("%dh%02dm", h, m%60)
}
