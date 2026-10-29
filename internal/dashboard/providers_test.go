package dashboard

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// costProvidersNow is the clock every provider fixture is read at.
var costProvidersNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// fptr is a figure's address, for the fixtures' optional fields.
func fptr(v float64) *float64 { return &v }

// deepSeekFixture is a spend report with placeholder figures: a top-up on
// 10-24, a day with no figure on 10-27, and today's figure on 10-29.
func deepSeekFixture(t *testing.T, runway float64) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"balance":                        40.0,
		"pace_1h":                        map[string]any{"per_hour": 0.05},
		"pace_24h":                       map[string]any{"per_hour": 0.2},
		"runway_days_at_24h_pace":        runway,
		"cash":                           30.0,
		"credit":                         10.0,
		"credit_runway_days_at_24h_pace": 4.0,
		"last_sample":                    "2026-10-29T11:55:00",
		"watcher_pid":                    nil,
		"live":                           map[string]any{"ok": false, "available": false},
		"credits": []map[string]any{
			{"at": "2026-10-20T08:00:00", "amount": 5.0, "kind": "credit"},
			{"at": "2026-10-24T09:00:00", "amount": 20.0, "kind": "top-up"},
			{"at": "2026-10-27T10:00:00", "amount": 10.0, "kind": "credit"},
		},
		"per_day": map[string]any{
			"2026-10-22": map[string]any{"spent": 4.0},
			"2026-10-23": map[string]any{"spent": 3.0},
			"2026-10-24": map[string]any{"spent": -20.0},
			"2026-10-25": map[string]any{"spent": 2.0},
			"2026-10-26": map[string]any{"spent": 2.5},
			"2026-10-27": map[string]any{"spent": nil},
			"2026-10-28": map[string]any{"spent": 3.5},
			"2026-10-29": map[string]any{"spent": 1.5},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// claudeFixture is a plan block and its charts with placeholder figures: the
// headline's today and 24-hour rate, and daily bars whose yesterday and mean
// the row reads.
func claudeFixture() (*ClaudePlan, *CostCharts) {
	plan := &ClaudePlan{
		WeeklyPct:           42,
		PacePerDay1h:        2.4,
		PacePerDay24h:       4.8,
		ProjectedPctAtReset: 75,
		WeeklyValueUSD:      50,
		HitsLimitInDays:     fptr(5.7),
		SampledAt:           costProvidersNow.Unix(),
		WeeklyResetsAt:      costProvidersNow.Add(48 * time.Hour).Unix(),
	}
	charts := &CostCharts{
		Headline: &CostHeadline{Today: fptr(3.0), PerHour24h: fptr(0.75)},
		Daily: &CostChart{Claude: &CostClaudeSeries{Bars: []CostBar{
			{Label: "23", Value: 1},
			{Label: "24", Gap: true},
			{Label: "25", Value: 2},
			{Label: "26", Value: 4},
			{Label: "27", Value: 6},
			{Label: "28", Value: 2.5},
			{Label: "29", Value: 3.1},
		}}},
	}
	return plan, charts
}

// sameDollars is whether two dollar figures agree to well under a cent.
func sameDollars(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// figureIs fails the test unless got holds want.
func figureIs(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s is nil, want %v", name, want)
		return
	}
	if !sameDollars(*got, want) {
		t.Errorf("%s = %v, want %v", name, *got, want)
	}
}

func TestCostProvidersDeepSeekFigures(t *testing.T) {
	t.Parallel()

	p := deepSeekCostProvider(costProvidersNow, deepSeekFixture(t, 2.5), nil, nil, nil)

	if p.ID != "deepseek" || p.Kind != "cash" {
		t.Errorf("row is %q %q, want deepseek cash", p.ID, p.Kind)
	}
	figureIs(t, "balance", p.Balance, 40)
	figureIs(t, "per_hour_1h", p.PerHour1h, 0.05)
	figureIs(t, "per_hour_24h", p.PerHour24h, 0.2)
	figureIs(t, "today", p.Today, 1.5)
	figureIs(t, "yesterday", p.Yesterday, 3.5)
	// The mean is of 10-22, 23, 25, 26 and 28: the top-up day, the day with no
	// figure and today are left out.
	figureIs(t, "avg_7d", p.Avg7d, 3.0)
	figureIs(t, "runway", p.Runway, 2.5)
	if p.Why != "" {
		t.Errorf("a readable report carries why %q", p.Why)
	}

	wantWarnings := []string{"live balance unreachable", "spend watcher is not running"}
	if !slices.Equal(p.Warnings, wantWarnings) {
		t.Errorf("warnings = %q, want %q", p.Warnings, wantWarnings)
	}

	for _, want := range []string{
		"DeepSeek $30.00 cash + $10.00 credit · credit lasts ~4.0d, then cash",
		"10-27 10:00 +$10.00 credit",
		"10-24 09:00 +$20.00 top-up",
		"ledger 2026-10-29 11:55:00",
	} {
		if !slices.Contains(p.Detail, want) {
			t.Errorf("detail %q has no %q", p.Detail, want)
		}
	}
	if slices.ContainsFunc(p.Detail, func(s string) bool { return strings.Contains(s, "10-20") }) {
		t.Errorf("detail %q carries the third-newest top-up, which the row leaves out", p.Detail)
	}
}

func TestCostProvidersDeepSeekStatusByRunway(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		runway float64
		want   string
	}{
		{2.5, "bad"},
		{5, "warn"},
		{9, "ok"},
	} {
		p := deepSeekCostProvider(costProvidersNow, deepSeekFixture(t, tc.runway), nil, nil, nil)
		if p.Status != tc.want {
			t.Errorf("runway %v days gives status %q, want %q", tc.runway, p.Status, tc.want)
		}
		if want := strconv.FormatFloat(tc.runway, 'f', 1, 64) + "d runway"; p.StatusText != want {
			t.Errorf("runway %v days gives status text %q, want %q", tc.runway, p.StatusText, want)
		}
	}
}

// The live warning fails closed: it takes a field that confirms the read to
// keep it away, and a report that says nothing about the live read is not
// evidence the balance was live. The page colors the balance bad on Available
// as well as alarming on OK, so either field failing raises the line.
func TestCostProvidersDeepSeekLiveWarningFailsClosed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		live map[string]any
		want bool
	}{
		{"both fields say live", map[string]any{"ok": true, "available": true}, false},
		{"only ok", map[string]any{"ok": true}, false},
		{"only available", map[string]any{"available": true}, false},
		{"ok false", map[string]any{"ok": false, "available": true}, true},
		{"available false without ok", map[string]any{"available": false}, true},
		{"ok true but available false", map[string]any{"ok": true, "available": false}, true},
		{"an empty live block", map[string]any{}, true},
		{"no live block", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report := map[string]any{"balance": 10.0, "watcher_pid": 4242}
			if tc.live != nil {
				report["live"] = tc.live
			}
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			p := deepSeekCostProvider(costProvidersNow, raw, nil, nil, nil)
			if got := slices.Contains(p.Warnings, "live balance unreachable"); got != tc.want {
				t.Errorf("live %v gives warnings %q, want the unreachable line: %v", tc.live, p.Warnings, tc.want)
			}
		})
	}
}

func TestCostProvidersDeepSeekTopUpDayIsNilWithTheRiseInDetail(t *testing.T) {
	t.Parallel()

	raw := deepSeekFixture(t, 9)
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	report["per_day"] = map[string]any{
		"2026-10-28": map[string]any{"spent": 3.5},
		"2026-10-29": map[string]any{"spent": -12.0},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}

	p := deepSeekCostProvider(costProvidersNow, raw, nil, nil, nil)
	if p.Today != nil {
		t.Errorf("today is %v for a day the balance rose, want nil", *p.Today)
	}
	figureIs(t, "yesterday", p.Yesterday, 3.5)
	if !slices.Contains(p.Detail, "2026-10-29 +$12.00 added") {
		t.Errorf("detail %q does not carry today's rise", p.Detail)
	}
}

func TestCostProvidersClaudeFigures(t *testing.T) {
	t.Parallel()

	plan, charts := claudeFixture()
	p := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)

	if p.ID != "claude" || p.Kind != "plan" {
		t.Errorf("row is %q %q, want claude plan", p.ID, p.Kind)
	}
	figureIs(t, "plan_pct", p.PlanPct, 42)
	// The headline's figures, so the table agrees with the charts.
	figureIs(t, "today", p.Today, 3.0)
	figureIs(t, "per_hour_24h", p.PerHour24h, 0.75)
	// The day's bars: yesterday is the 10-28 bar, and the mean skips the gap
	// and today.
	figureIs(t, "yesterday", p.Yesterday, 2.5)
	figureIs(t, "avg_7d", p.Avg7d, 3.1)
	// 2.4 points a day is 0.1 point an hour: a percent of the week's 50 dollars.
	figureIs(t, "per_hour_1h", p.PerHour1h, 0.05)
	if !slices.Contains(p.Detail, "plan pace last 24h $0.10/h, approximate") {
		t.Errorf("detail %q does not carry the plan pace's dollars", p.Detail)
	}
	if p.StatusText != "limit in 5.7d" {
		t.Errorf("status text = %q, want %q", p.StatusText, "limit in 5.7d")
	}
	if p.Status != "ok" {
		t.Errorf("status = %q for a week at 42%% projected to 75%%, want ok", p.Status)
	}
}

// The five-hour limit is in the hover with its reset; a sample that carried no
// reset says so instead of drawing a time.
func TestCostProvidersClaudeDetailCarriesTheFiveHourLimit(t *testing.T) {
	t.Parallel()

	// Built in local zone so the wall clock the hover shows is 15:04 wherever
	// the test runs; 29 October 2026 is a Thursday.
	reset := time.Date(2026, time.October, 29, 15, 4, 0, 0, time.Local).Unix()
	for _, tc := range []struct {
		name  string
		reset int64
		want  string
	}{
		{"with a reset", reset, "5-hour limit 30% used, resets Thu 15:04"},
		{"without a reset", 0, "5-hour limit 30% used, no reset instant in the newest sample"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plan, charts := claudeFixture()
			plan.FiveHourPct = 30
			plan.FiveHourResetsAt = tc.reset
			p := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)

			if !slices.Contains(p.Detail, tc.want) {
				t.Errorf("detail %q does not carry %q", p.Detail, tc.want)
			}
		})
	}
}

// The API-equivalent dollars are in the hover only when the plan has both; an
// unpriced model is named after them, since they are then a floor.
func TestCostProvidersClaudeDetailCarriesTheAPIEquivalent(t *testing.T) {
	t.Parallel()

	plan, charts := claudeFixture()
	plan.APIEquivalentWeekUSD = fptr(12.5)
	plan.APIEquivalentMonthUSD = fptr(180)
	plan.Unpriced = []string{"placeholder-model"}
	p := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)

	want := "API-equivalent $12.50 this week, $180.00 this month, approximate: plans weight tokens differently from the API · counted at zero, no list price: placeholder-model"
	if !slices.Contains(p.Detail, want) {
		t.Errorf("detail %q does not carry %q", p.Detail, want)
	}

	// A week without its month is not a figure the hover can state, so neither
	// is drawn.
	for name, set := range map[string]func(*ClaudePlan){
		"no figures": func(*ClaudePlan) {},
		"week only":  func(p *ClaudePlan) { p.APIEquivalentWeekUSD = fptr(12.5) },
		"month only": func(p *ClaudePlan) { p.APIEquivalentMonthUSD = fptr(180) },
	} {
		plan, charts := claudeFixture()
		set(plan)
		p := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)
		for _, line := range p.Detail {
			if strings.Contains(line, "API-equivalent") {
				t.Errorf("%s: detail draws %q, want no API-equivalent line", name, line)
			}
		}
	}
}

// The billing month's use and its weeks at the ceiling are in the hover, and a
// month with no counted week draws neither.
func TestCostProvidersClaudeDetailCarriesTheMonth(t *testing.T) {
	t.Parallel()

	plan, charts := claudeFixture()
	plan.WeeksCounted = 3
	plan.UtilizationPct = 57.4
	plan.WeeksAtCeiling = 2
	p := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)

	if !slices.Contains(p.Detail, "month 57% of plan · 2 weeks at ceiling") {
		t.Errorf("detail %q does not carry the month's use and its weeks at the ceiling", p.Detail)
	}

	// A counted month with no week at the ceiling says its use alone; one week
	// at the ceiling is named in the singular.
	plan, charts = claudeFixture()
	plan.WeeksCounted = 3
	plan.UtilizationPct = 57.4
	p = claudeCostProvider(costProvidersNow, nil, plan, charts, nil)
	if !slices.Contains(p.Detail, "month 57% of plan") {
		t.Errorf("detail %q does not carry the month's use alone", p.Detail)
	}

	plan.WeeksAtCeiling = 1
	p = claudeCostProvider(costProvidersNow, nil, plan, charts, nil)
	if !slices.Contains(p.Detail, "month 57% of plan · 1 week at ceiling") {
		t.Errorf("detail %q does not name the one week at the ceiling", p.Detail)
	}

	plan, charts = claudeFixture()
	p = claudeCostProvider(costProvidersNow, nil, plan, charts, nil)
	for _, line := range p.Detail {
		if strings.HasPrefix(line, "month ") {
			t.Errorf("detail draws %q for a month with no counted week", line)
		}
	}
}

// A headline whose hourly rate read fewer hours than its window says how many
// it read; a full window says nothing.
func TestCostProvidersClaudeDetailCarriesTheHoursRead(t *testing.T) {
	t.Parallel()

	plan, charts := claudeFixture()
	charts.Headline.WindowHours = 24
	charts.Headline.HoursRead = 18
	p := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)

	if !slices.Contains(p.Detail, "18 of 24 hours read") {
		t.Errorf("detail %q does not state the hours read", p.Detail)
	}

	// No hour read at all is the emptiest headline, and must say so too.
	charts.Headline.HoursRead = 0
	p = claudeCostProvider(costProvidersNow, nil, plan, charts, nil)

	if !slices.Contains(p.Detail, "0 of 24 hours read") {
		t.Errorf("detail %q does not state that no hours were read", p.Detail)
	}

	charts.Headline.HoursRead = 24
	p = claudeCostProvider(costProvidersNow, nil, plan, charts, nil)
	for _, line := range p.Detail {
		if strings.Contains(line, "hours read") {
			t.Errorf("detail draws %q for a full window", line)
		}
	}
}

// A poll that could not keep its state says so in the hover, so a history that
// stopped growing is not read as a healthy one.
func TestCostProvidersClaudeDetailCarriesTheStateError(t *testing.T) {
	t.Parallel()

	plan, charts := claudeFixture()
	plan.StateError = "the closeout history could not be written"
	p := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)

	if !slices.Contains(p.Detail, "state not kept: the closeout history could not be written") {
		t.Errorf("detail %q does not carry the state error", p.Detail)
	}

	plan, charts = claudeFixture()
	p = claudeCostProvider(costProvidersNow, nil, plan, charts, nil)
	for _, line := range p.Detail {
		if strings.HasPrefix(line, "state not kept") {
			t.Errorf("detail draws %q for a poll that kept its state", line)
		}
	}
}

// The row's status is the colour the table's chip takes, and the rule is the
// reader's own: a week at the ceiling, or a projection past the limit, is bad; a
// projection past the ceiling is a warning; anything else is ok. The cases pin
// each edge against the constant, so a ceiling that drifts on either side turns
// this red: a week exactly at the ceiling and a projection one point past it are
// both rows.
func TestCostProvidersClaudeStatusFollowsTheCeiling(t *testing.T) {
	t.Parallel()

	ceiling := float64(ClaudePlanCeilingPct)
	for _, tc := range []struct {
		name              string
		weekly, projected float64
		want              string
	}{
		{"projection under the ceiling", 42, 75, "ok"},
		{"projection just under the ceiling", 42, ceiling - 0.1, "ok"},
		{"projection one point past the ceiling", 42, ceiling + 0.5, "warn"},
		{"projection at the limit", 42, 100, "warn"},
		{"projection past the limit", 42, 101, "bad"},
		{"week just under the ceiling", ceiling - 0.1, 50, "ok"},
		{"week exactly at the ceiling", ceiling, 50, "bad"},
		{"week and projection both past", 95, 120, "bad"},
		{"nothing used yet", 0, 0, "ok"},
	} {
		plan, charts := claudeFixture()
		plan.WeeklyPct = tc.weekly
		plan.ProjectedPctAtReset = tc.projected
		if got := claudeCostProvider(costProvidersNow, nil, plan, charts, nil).Status; got != tc.want {
			t.Errorf("%s: weekly %v projected %v gives status %q, want %q", tc.name, tc.weekly, tc.projected, got, tc.want)
		}
	}
}

// The Claude mean is the chart's week, not the whole series: a bar list longer
// than the week averages its newest seven complete days.
func TestCostProvidersClaudeAverageCapsAtSevenDays(t *testing.T) {
	t.Parallel()

	plan, charts := claudeFixture()
	bars := []CostBar{{Label: "21", Gap: true}}
	for i, v := range []float64{1, 2, 3, 4, 5, 6, 7, 8} {
		bars = append(bars, CostBar{Label: strconv.Itoa(22 + i), Value: v})
	}
	bars = append(bars, CostBar{Label: "30", Value: 9})
	charts.Daily.Claude.Bars = bars

	p := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)
	// The newest seven complete days before today are 2 to 8: 1 is older than
	// the cap, and today's 9 is never in the mean.
	figureIs(t, "avg_7d", p.Avg7d, 5)
	figureIs(t, "yesterday", p.Yesterday, 8)
}

func TestCostProvidersUnavailableHasNilFiguresAndWhy(t *testing.T) {
	t.Parallel()

	plan, charts := claudeFixture()
	plan.Unavailable = true
	claude := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)

	plan, charts = claudeFixture()
	plan.SampledAt = costProvidersNow.Add(-2 * time.Hour).Unix()
	plan.Stale = true
	stale := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)

	for _, p := range []CostProvider{
		claude,
		stale,
		deepSeekCostProvider(costProvidersNow, nil, nil, nil, nil),
		deepSeekCostProvider(costProvidersNow, json.RawMessage("null"), nil, nil, nil),
		jevCostProvider(costProvidersNow, nil, nil, nil, nil),
	} {
		if p.Why == "" {
			t.Errorf("%s has no why", p.ID)
		}
		for name, v := range map[string]*float64{
			"balance": p.Balance, "plan_pct": p.PlanPct, "today": p.Today,
			"yesterday": p.Yesterday, "avg_7d": p.Avg7d, "per_hour_1h": p.PerHour1h,
			"per_hour_24h": p.PerHour24h, "runway": p.Runway,
		} {
			if v != nil {
				t.Errorf("%s carries %s = %v while unavailable, want nil", p.ID, name, *v)
			}
		}
		if p.Status != "none" {
			t.Errorf("%s status = %q while unavailable, want none", p.ID, p.Status)
		}
	}
	if want := "the status line's samples could not be read"; claude.Why != want {
		t.Errorf("claude why = %q, want %q", claude.Why, want)
	}
	if want := "the newest sample is 2h00m old"; stale.Why != want {
		t.Errorf("stale claude why = %q, want %q", stale.Why, want)
	}
}

func TestCostProvidersJevRow(t *testing.T) {
	t.Parallel()

	jev := &JevSpend{Calls: 12, USD: 3.6, Window: "24h", TopCaller: "placeholder-caller", TopCallerCalls: 5, TopCallerUSD: 1.5}
	p := jevCostProvider(costProvidersNow, nil, nil, nil, jev)
	if p.ID != "jev" || p.Kind != "cash" {
		t.Errorf("row is %q %q, want jev cash", p.ID, p.Kind)
	}
	figureIs(t, "per_hour_24h", p.PerHour24h, 0.15)
	if p.Today != nil {
		t.Errorf("jev has a today figure %v, want nil", *p.Today)
	}
	if want := "12 calls in 24h"; len(p.Detail) == 0 || p.Detail[0] != want {
		t.Errorf("detail %q, want it to start with %q", p.Detail, want)
	}
	if want := "top caller placeholder-caller (5 calls, $1.50)"; !slices.Contains(p.Detail, want) {
		t.Errorf("detail %q does not carry %q", p.Detail, want)
	}

	// The rate needs a day's window, and the spelling of the window is not what
	// decides it: the reader writes "24h", and a writer's days or bare-hours
	// form must not silently drop the rate.
	for _, tc := range []struct {
		window string
		want   *float64
	}{
		{"24h", fptr(0.15)},
		{"24h0m0s", fptr(0.15)},
		{"1d", fptr(0.15)},
		{"24", fptr(0.15)},
		{"1h", nil},
		{"2d", nil},
		{"", nil},
		{"yesterday", nil},
	} {
		jev.Window = tc.window
		p := jevCostProvider(costProvidersNow, nil, nil, nil, jev)
		switch {
		case tc.want == nil && p.PerHour24h != nil:
			t.Errorf("window %q gives a 24-hour rate %v, want nil", tc.window, *p.PerHour24h)
		case tc.want != nil:
			figureIs(t, "per_hour_24h for window "+tc.window, p.PerHour24h, *tc.want)
		}
	}
}

// costProviderRow is the row with id in a built list, which a test names rather
// than counting rows.
func costProviderRow(t *testing.T, rows []CostProvider, id string) CostProvider {
	t.Helper()
	for _, p := range rows {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no %s row in %d providers", id, len(rows))
	return CostProvider{}
}

// A Jev read that failed or merely aged keeps its row, but the row says why and
// carries no figures: the last good ones would read as today's. They stay in
// the row's hover. The row is built against the hub's own read stamps, so it
// follows a read that fails and one that is only old.
func TestCostProvidersJevRowSaysWhyWhenItsReadIsNotCurrent(t *testing.T) {
	t.Parallel()

	// A read may age three of its own polls (five minutes each) before the hub
	// calls it stale.
	staleAge := 16 * time.Minute
	for _, tc := range []struct {
		name string
		fail bool
		age  time.Duration
		want string
	}{
		{name: "failed", fail: true, want: "the read failed"},
		{name: "stale", age: staleAge, want: "the read went stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			now := costProvidersNow
			fail := false
			raw := deepSeekFixture(t, 9)
			h := NewHub(Config{
				Now:   func() time.Time { return now },
				Spend: func() json.RawMessage { return raw },
				JevSpend: func() *JevSpend {
					if fail {
						return nil
					}
					return &JevSpend{Calls: 12, USD: 3.6, Window: "24h", TopCaller: "placeholder-caller", TopCallerCalls: 5, TopCallerUSD: 1.5}
				},
			})
			h.pollSpend()
			h.pollJevSpend()
			if p := costProviderRow(t, h.State().CostProviders, costProviderJev); p.Why != "" {
				t.Fatalf("a current Jev read draws the row marked %q", p.Why)
			}

			fail = tc.fail
			now = now.Add(tc.age)
			if tc.fail {
				h.pollJevSpend()
			}

			st := h.State()
			p := costProviderRow(t, st.CostProviders, costProviderJev)
			if p.Why != tc.want {
				t.Errorf("Jev why = %q, want %q", p.Why, tc.want)
			}
			for name, v := range map[string]*float64{
				"balance": p.Balance, "plan_pct": p.PlanPct, "today": p.Today,
				"yesterday": p.Yesterday, "avg_7d": p.Avg7d, "per_hour_1h": p.PerHour1h,
				"per_hour_24h": p.PerHour24h, "runway": p.Runway,
			} {
				if v != nil {
					t.Errorf("the Jev row carries %s = %v while its read is not current, want nil", name, *v)
				}
			}
			if p.Status != "none" {
				t.Errorf("the Jev row's status is %q, want none", p.Status)
			}
			for _, want := range []string{
				"last good: $3.60 over 24h · $0.15/h",
				"12 calls in 24h",
				"top caller placeholder-caller (5 calls, $1.50)",
			} {
				if !slices.Contains(p.Detail, want) {
					t.Errorf("the Jev row's hover %q does not carry %q", p.Detail, want)
				}
			}
			if deep := costProviderRow(t, st.CostProviders, costProviderDeepSeek); deep.Why != "" || deep.Balance == nil {
				t.Errorf("the Jev read's state changed the DeepSeek row: %+v", deep)
			}
		})
	}
}

func TestCostProvidersOrderIsFixed(t *testing.T) {
	t.Parallel()

	plan, charts := claudeFixture()
	rows := BuildCostProviders(costProvidersNow, deepSeekFixture(t, 9), plan, charts, &JevSpend{Calls: 1, USD: 1})
	var ids []string
	for _, p := range rows {
		ids = append(ids, p.ID)
	}
	if want := []string{"deepseek", "claude", "jev"}; !slices.Equal(ids, want) {
		t.Errorf("provider order = %q, want %q", ids, want)
	}
}

// The state carries the rows in the order the builders list them, and a builder
// added to the hub's list shows up in the state the page reads.
func TestCostProvidersStateCarriesTheRowsInOrder(t *testing.T) {
	t.Parallel()

	fake := func(time.Time, json.RawMessage, *ClaudePlan, *CostCharts, *JevSpend) CostProvider {
		return CostProvider{ID: "fake", Label: "Fake", Kind: "cash", Status: "none", Why: "placeholder"}
	}
	raw := deepSeekFixture(t, 9)
	plan, _ := claudeFixture()
	jev := &JevSpend{Calls: 2, USD: 0.5, Window: "24h"}
	h := NewHub(Config{
		Now:        func() time.Time { return costProvidersNow },
		Spend:      func() json.RawMessage { return raw },
		ClaudePlan: func() *ClaudePlan { return plan },
		JevSpend:   func() *JevSpend { return jev },
	})
	h.costProviders = append(slices.Clip(costProviderBuilders), fake)
	h.pollSpend()
	h.pollClaudePlan()
	h.pollJevSpend()

	var ids []string
	for _, p := range h.State().CostProviders {
		ids = append(ids, p.ID)
	}
	if want := []string{"deepseek", "claude", "jev", "fake"}; !slices.Equal(ids, want) {
		t.Errorf("state providers = %q, want %q", ids, want)
	}

	body, err := json.Marshal(h.State())
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		CostProviders []struct {
			ID string `json:"id"`
		} `json:"cost_providers"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	var wireIDs []string
	for _, p := range wire.CostProviders {
		wireIDs = append(wireIDs, p.ID)
	}
	if want := []string{"deepseek", "claude", "jev", "fake"}; !slices.Equal(wireIDs, want) {
		t.Errorf("cost_providers on the wire = %q, want %q", wireIDs, want)
	}
}

// A town with no provider reader still sends cost_providers, as an empty list:
// the page reads a missing key as a reading that did not arrive, not as none.
func TestCostProvidersSendsAnEmptyListWhenNoReaderIsConfigured(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{Now: func() time.Time { return costProvidersNow }})

	body, err := json.Marshal(h.State())
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		CostProviders *[]json.RawMessage `json:"cost_providers"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.CostProviders == nil {
		t.Fatal("cost_providers is missing from the wire, want an empty list")
	}
	if n := len(*wire.CostProviders); n != 0 {
		t.Errorf("cost_providers on the wire has %d rows, want none", n)
	}
}

func TestCostProvidersOmitsAReaderThatIsNotConfigured(t *testing.T) {
	t.Parallel()

	raw := deepSeekFixture(t, 9)
	h := NewHub(Config{
		Now:   func() time.Time { return costProvidersNow },
		Spend: func() json.RawMessage { return raw },
	})
	h.pollSpend()

	var ids []string
	for _, p := range h.State().CostProviders {
		ids = append(ids, p.ID)
	}
	if want := []string{"deepseek"}; !slices.Equal(ids, want) {
		t.Errorf("providers with only the spend reader = %q, want %q", ids, want)
	}
}
