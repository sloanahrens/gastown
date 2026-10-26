package dashboard

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// costRenderCount is what the drawn Cost panel holds: the bars of each chart and
// the limit lines, counted in the element tree the page built (gt-cmy73).
type costRenderCount struct {
	Daily  int `json:"daily"`
	Gap    int `json:"gap"`
	Hourly int `json:"hourly"`
	Limit  int `json:"limit"`
}

// costRenderJS walks the element tree under #cost and counts the bars of the
// daily and hourly charts and the limit lines, by the class each element carries.
const costRenderJS = `(function () {
  var out = {daily: 0, hourly: 0, limit: 0, gap: 0};
  function walk(n, chart) {
    if (!n || !n.children) return;
    for (var i = 0; i < n.children.length; i++) {
      var c = n.children[i];
      var cls = c.attr_class || "";
      var here = chart;
      if (cls.indexOf("dchart") >= 0) here = cls.indexOf("hourly") >= 0 ? "hourly" : "daily";
      if (cls === "bar" && here) out[here]++;
      if (cls === "limit") out.limit++;
      if (cls === "gap") out.gap++;
      walk(c, here);
    }
  }
  walk(document.getElementById("cost"), "");
  return JSON.stringify(out);
})()`

// The Cost panel draws what the server built: the daily chart's bars and its
// weekly limit line, and the hourly chart's bars, counted in the drawn element
// tree from fixture records (gt-cmy73).
func TestCostPanelDrawsItsChartsFromTheServersData(t *testing.T) {
	t.Parallel()

	perDay := map[string]float64{
		"2026-10-19": 1.00, "2026-10-20": 2.00, "2026-10-21": 1.50, "2026-10-22": 0.50,
		"2026-10-23": 3.00, "2026-10-24": 2.25, "2026-10-25": 4.00,
	}
	first := time.Date(2026, 10, 24, 17, 0, 0, 0, time.Local)
	hours := make([]SpendHour, spendHours)
	for i := range hours {
		hours[i] = SpendHour{Start: first.Add(time.Duration(i) * time.Hour), Samples: 2}
	}
	hours[1].Spent = 0.40
	hours[5].Spent = -0.50                                // a top-up: a rise is money added, drawn as a zero bar, not a gap
	hours[3] = SpendHour{Start: first.Add(3 * time.Hour)} // no sample: a gap
	plan := &ClaudePlan{WeeklyValueUSD: 70, ProjectedPctAtReset: 83}
	charts := BuildCostCharts(time.Date(2026, 10, 25, 16, 30, 0, 0, time.Local), perDay, hours, 0, plan)

	spend, err := json.Marshal(map[string]any{
		"live":         map[string]any{"ok": true, "available": true},
		"balance":      77.49,
		"spent_ledger": 12.34,
		"ledger_since": "2026-10-19T00:00",
		"per_day": func() map[string]any {
			out := map[string]any{}
			for day, v := range perDay {
				out[day] = map[string]any{"spent": v}
			}
			return out
		}(),
	})
	if err != nil {
		t.Fatal(err)
	}
	state := jsonValue(t, map[string]any{
		"spend":       json.RawMessage(spend),
		"claude_plan": plan,
		"cost_charts": charts,
	})

	p := newPageDOM(t)
	p.call(t, "renderCost", state)
	var got costRenderCount
	counted, err := p.vm.RunString(costRenderJS)
	if err != nil {
		t.Fatalf("running the drawn-panel count: %v", err)
	}
	if err := json.Unmarshal([]byte(counted.String()), &got); err != nil {
		t.Fatalf("reading the drawn Cost panel: %v", err)
	}

	if want := len(charts.Daily.Bars); got.Daily != want {
		t.Errorf("daily bars drawn = %d, want the server's %d", got.Daily, want)
	}
	if want := spendHours; got.Hourly != want-1 {
		// One hour has no sample: a gap, drawn as a tick, not a bar.
		t.Errorf("hourly bars drawn = %d, want %d (the gap is not a bar)", got.Hourly, want-1)
	}
	if got.Limit != 1 {
		t.Errorf("limit lines drawn = %d, want the weekly limit line", got.Limit)
	}
	if got.Gap != 1 {
		t.Errorf("gap ticks drawn = %d, want the one hour with no sample", got.Gap)
	}
	if charts.Daily.Limit == nil || math.Abs(charts.Daily.Limit.Value-10) > 1e-9 {
		t.Errorf("the server's limit = %+v, want 10 a day", charts.Daily.Limit)
	}
}

// jsonValue is v as the page would receive it from the server: marshalled and
// decoded, so the page reads the JSON names the server writes, not Go's field
// names.
func jsonValue(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The headline row above the charts reads the server's spend figures: the
// balance and the spent total the report carries (gt-59v3x).
func TestCostPanelHeadlineReadsTheServersFigures(t *testing.T) {
	t.Parallel()

	spend, err := json.Marshal(map[string]any{
		"live":         map[string]any{"ok": true, "available": true},
		"balance":      77.49,
		"spent_ledger": 12.34,
		"ledger_since": "2026-10-19T00:00",
		"per_day":      map[string]any{"2026-10-25": map[string]any{"spent": 4.0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := newPageDOM(t)
	p.call(t, "renderCost", jsonValue(t, map[string]any{"spend": json.RawMessage(spend)}))
	text, err := p.vm.RunString(`document.getElementById("cost").textContent`)
	if err != nil {
		t.Fatalf("reading the drawn Cost panel: %v", err)
	}
	for _, want := range []string{"$77.49", "$12.34"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the Cost headline does not show %s from the server's figures:\n%s", want, text.String())
		}
	}
}
