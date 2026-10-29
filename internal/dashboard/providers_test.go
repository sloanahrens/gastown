package dashboard

import (
	"encoding/json"
	"math"
	"regexp"
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

func TestCostProvidersClaudeStatusMatchesPlanClass(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		weekly, projected float64
		want              string
	}{
		{42, 75, "ok"},
		{42, 95, "warn"},
		{90, 50, "bad"},
		{50, 101, "bad"},
	} {
		plan, charts := claudeFixture()
		plan.WeeklyPct = tc.weekly
		plan.ProjectedPctAtReset = tc.projected
		p := claudeCostProvider(costProvidersNow, nil, plan, charts, nil)
		if p.Status != tc.want {
			t.Errorf("weekly %v projected %v gives status %q, want %q", tc.weekly, tc.projected, p.Status, tc.want)
		}
	}
}

// The row's ceiling is the reader's, and the page's copy must equal it: the
// page colours a week by its own CLAUDE_PLAN_CEILING_PCT.
func TestCostProvidersCeilingIsThePagesCeiling(t *testing.T) {
	t.Parallel()

	m := regexp.MustCompile(`const CLAUDE_PLAN_CEILING_PCT = (\d+);`).FindSubmatch(indexHTML)
	if m == nil {
		t.Fatal("index.html has no CLAUDE_PLAN_CEILING_PCT constant")
	}
	if got, _ := strconv.ParseFloat(string(m[1]), 64); got != ClaudePlanCeilingPct {
		t.Errorf("the page's ceiling is %v, the reader's is %v", got, ClaudePlanCeilingPct)
	}
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

	jev.Window = "1h"
	if p := jevCostProvider(costProvidersNow, nil, nil, nil, jev); p.PerHour24h != nil {
		t.Errorf("a one-hour window gives a 24-hour rate %v, want nil", *p.PerHour24h)
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
	h.costProviders = append(slices.Clip(h.costProviders), fake)
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
