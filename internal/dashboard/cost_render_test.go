package dashboard

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
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
	charts := BuildCostCharts(time.Date(2026, 10, 25, 16, 30, 0, 0, time.Local), perDay, hours, 0, plan, PlanSamples{})

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

// costClaudeDraw is what a reader sees of the drawn Cost panel: every tooltip
// under it, the charts' aria-labels, and the panel's text (where the legend
// lives). It holds no class name, so a page that draws the series in different
// elements still has to say the same things.
type costClaudeDraw struct {
	Titles []string `json:"titles"`
	Aria   []string `json:"aria"`
	Text   string   `json:"text"`
}

// costClaudeJS reads back the tooltips, aria-labels and words of the drawn Cost
// panel, the way a reader hovering the chart or a screen reader has them.
const costClaudeJS = `(function () {
  var out = {titles: [], aria: [], text: ""};
  function walk(n) {
    if (!n || !n.children) return;
    for (var i = 0; i < n.children.length; i++) {
      var c = n.children[i];
      if (String(c.tagName || "").toUpperCase() === "TITLE") out.titles.push(String(c.textContent));
      var label = c.getAttribute ? c.getAttribute("aria-label") : null;
      if (label) out.aria.push(String(label));
      walk(c);
    }
  }
  var root = document.getElementById("cost");
  walk(root);
  out.text = root.textContent;
  return JSON.stringify(out);
})()`

// costClaudeDrawn renders the Cost panel over one payload and reads back what a
// reader sees of it.
func costClaudeDrawn(t *testing.T, p *pageDOM) costClaudeDraw {
	t.Helper()

	drawn, err := p.vm.RunString(costClaudeJS)
	if err != nil {
		t.Fatalf("reading the drawn Cost panel: %v", err)
	}
	var got costClaudeDraw
	if err := json.Unmarshal([]byte(drawn.String()), &got); err != nil {
		t.Fatalf("reading the drawn Cost panel: %v", err)
	}
	return got
}

// costRenderPayload is the spend report the Cost panel draws beside a charts
// payload: the two days the fixtures' Claude series covers.
func costRenderPayload(t *testing.T, days map[string]float64) json.RawMessage {
	t.Helper()

	perDay := map[string]any{}
	for day, spent := range days {
		perDay[day] = map[string]any{"spent": spent}
	}
	spend, err := json.Marshal(map[string]any{
		"live":    map[string]any{"ok": true, "available": true},
		"balance": 77.49,
		"per_day": perDay,
	})
	if err != nil {
		t.Fatal(err)
	}
	return spend
}

// The Cost panel draws the Claude series inside the same daily chart as the
// DeepSeek bars, with a legend that carries the plan-equivalent label, a tooltip
// on every bucket it drew, and a hatch on the buckets the series has no sample
// for instead of a bar of zero (gt-yxx98).
func TestCostPanelDrawsTheClaudeSeriesInTheSameChart(t *testing.T) {
	t.Parallel()

	days := map[string]float64{"2026-10-19": 1.00, "2026-10-20": 2.00}
	day := func(h int) time.Time { return time.Date(2026, 10, 19, h, 0, 0, 0, time.Local) }
	charts := BuildCostCharts(time.Date(2026, 10, 25, 16, 30, 0, 0, time.Local), days, nil, 0, claudeSeriesPlan,
		claudeSeries(claudeSampleAt(day(9), 10), claudeSampleAt(day(10), 12)))
	if charts.Daily == nil || charts.Daily.Claude == nil {
		t.Fatalf("the fixture built no Claude series on the daily chart: %+v", charts.Daily)
	}

	// The legend is built from the providers the server sends, so the two rows
	// that own the two series are sent with the charts.
	p := newPageDOM(t)
	p.call(t, "renderCost", jsonValue(t, map[string]any{
		"spend":          costRenderPayload(t, days),
		"cost_charts":    charts,
		"cost_providers": []any{deepSeekProvider(), claudeProvider()},
	}))
	got := costClaudeDrawn(t, p)

	// Every bucket's mark is hoverable, and the tooltip says what the server
	// charged that bucket: a bar's dollars, or that the series has no sample.
	for _, b := range charts.Daily.Claude.Bars {
		found := false
		for _, tip := range got.Titles {
			if strings.Contains(tip, claudePlanSeriesName) && strings.HasSuffix(tip, b.Title) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no tooltip for the %s bucket %q among %v", b.Label, b.Title, got.Titles)
		}
	}
	gaps := 0
	for _, b := range charts.Daily.Claude.Bars {
		if b.Gap {
			gaps++
		}
	}
	if gaps == 0 {
		t.Error("the fixture has no bucket without a sample, so the hatch is untested")
	}
	if !strings.Contains(got.Text, "DeepSeek") || !strings.Contains(got.Text, claudePlanSeriesName) {
		t.Errorf("the panel's legend does not name both series %q:\n%s", claudePlanSeriesName, got.Text)
	}
	named := 0
	for _, label := range got.Aria {
		if strings.Contains(label, claudePlanSeriesName) {
			named++
			if !strings.Contains(label, "bucket") {
				t.Errorf("the chart's aria-label %q does not count the buckets with no sample", label)
			}
		}
	}
	if named == 0 {
		t.Errorf("no chart's aria-label carries %q: %v", claudePlanSeriesName, got.Aria)
	}
}

// A chart whose samples could not be read says so on the panel instead of
// drawing a series of zeros, and so does one the page has to drop because it
// does not line up with the chart's buckets: a chart without Claude dollars is
// never silent about why (gt-yxx98).
func TestCostPanelSaysWhyTheClaudeSeriesIsAbsent(t *testing.T) {
	t.Parallel()

	days := map[string]float64{"2026-10-25": 2.00}
	now := time.Date(2026, 10, 25, 16, 30, 0, 0, time.Local)
	unreadable := BuildCostCharts(now, days, nil, 0, claudeSeriesPlan, PlanSamples{Read: true, Failed: true})
	// The same chart with a series the page cannot place: one bucket short of
	// the bars it would be drawn beside.
	misaligned := BuildCostCharts(now, days, nil, 0, claudeSeriesPlan,
		claudeSeries(claudeSampleAt(time.Date(2026, 10, 25, 9, 0, 0, 0, time.Local), 40)))
	if misaligned.Daily == nil || misaligned.Daily.Claude == nil {
		t.Fatalf("the fixture built no Claude series to misalign: %+v", misaligned.Daily)
	}
	misaligned.Daily.Claude.Bars = misaligned.Daily.Claude.Bars[:len(misaligned.Daily.Claude.Bars)-1]
	// The page draws the series array, so the short series is the one it reads.
	for i := range misaligned.Daily.Series {
		if s := &misaligned.Daily.Series[i]; s.ID == costProviderClaude {
			s.Bars = s.Bars[:len(s.Bars)-1]
		}
	}

	for _, tc := range []struct {
		name   string
		charts CostCharts
		want   string
	}{
		{"unreadable samples", unreadable, "could not be read"},
		{"a series the page cannot place", misaligned, "does not line up"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := newPageDOM(t)
			p.call(t, "renderCost", jsonValue(t, map[string]any{
				"spend":       costRenderPayload(t, days),
				"cost_charts": tc.charts,
			}))
			got := costClaudeDrawn(t, p)
			if !strings.Contains(got.Text, tc.want) {
				t.Errorf("the Cost panel does not say %q about the Claude series:\n%s", tc.want, got.Text)
			}
			for _, tip := range got.Titles {
				if strings.Contains(tip, claudePlanSeriesName) {
					t.Errorf("the panel drew a Claude tooltip %q it said it could not draw", tip)
				}
			}
		})
	}
}

// costBarGeom is one mark the page drew in a Cost chart: a bar, a gap tick or a
// hatched gap, with where it sits, its fill and the tooltip it carries.
type costBarGeom struct {
	Tag    string  `json:"tag"`
	Class  string  `json:"class"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
	Style  string  `json:"style"`
	Title  string  `json:"title"`
}

// costDrawn is what the page drew for one daily Cost chart: its marks in the order
// drawn, the entries of its legend, and every word the panel holds.
type costDrawn struct {
	Bars     []costBarGeom `json:"bars"`
	Amounts  []string      `json:"amounts"`
	Patterns int           `json:"patterns"`
	Legend   []string      `json:"legend"`
	Text     string        `json:"text"`
}

// costBarsJS reads back the marks under the Cost panel (bars, gap ticks and hatched
// gaps), the daily amounts above them, the legend's entries and the panel's text,
// the way a reader sees them.
const costBarsJS = `(function () {
  var out = {bars: [], amounts: [], patterns: 0, legend: [], text: ""};
  var marks = {bar: 1, cbar: 1, cgap: 1, gap: 1};
  function attr(n, k) { var v = n.getAttribute ? n.getAttribute(k) : null; return v == null ? "" : String(v); }
  function num(n, k) { return Number(attr(n, k)) || 0; }
  function walk(n) {
    if (!n || !n.children) return;
    for (var i = 0; i < n.children.length; i++) {
      var c = n.children[i];
      var cls = attr(c, "class");
      if (marks[cls]) {
        var tip = "";
        for (var j = 0; j < c.children.length; j++) {
          if (String(c.children[j].tagName || "").toUpperCase() === "TITLE") tip = String(c.children[j].textContent);
        }
        out.bars.push({tag: String(c.tagName).toUpperCase(), class: cls, x: num(c, "x"), y: num(c, "y"), width: num(c, "width"), height: num(c, "height"), style: attr(c, "style"), title: tip});
      }
      if (cls === "amt") out.amounts.push(String(c.textContent));
      if (String(c.tagName || "").toUpperCase() === "PATTERN") out.patterns++;
      if (String(c.className || "") === "legend") {
        for (var k = 0; k < c.children.length; k++) out.legend.push(String(c.children[k].textContent));
      }
      walk(c);
    }
  }
  var root = document.getElementById("cost");
  walk(root);
  out.text = String(root.textContent);
  return JSON.stringify(out);
})()`

// costDailyDrawn renders the Cost panel over one daily chart and the providers
// the server sends with it, and reads back what the page drew.
func costDailyDrawn(t *testing.T, chart map[string]any, providers []any) costDrawn {
	t.Helper()

	state := map[string]any{
		"spend":       costRenderPayload(t, nil),
		"cost_charts": map[string]any{"daily": chart},
	}
	if providers != nil {
		state["cost_providers"] = providers
	}
	p := newPageDOM(t)
	p.call(t, "renderCost", jsonValue(t, state))
	read, err := p.vm.RunString(costBarsJS)
	if err != nil {
		t.Fatalf("reading the drawn Cost chart: %v", err)
	}
	var got costDrawn
	if err := json.Unmarshal([]byte(read.String()), &got); err != nil {
		t.Fatalf("reading the drawn Cost chart: %v", err)
	}
	return got
}

// costBarFix is one bucket of a fixture series: a bar of value dollars.
func costBarFix(label string, value float64) map[string]any {
	return map[string]any{"label": label, "value": value, "title": fmt.Sprintf("%s $%.2f", label, value)}
}

// costGapFix is one bucket of a fixture series with no sample.
func costGapFix(label string) map[string]any {
	return map[string]any{"label": label, "value": 0, "title": label + " no sample", "gap": true}
}

// costSeriesFix is one series of a fixture chart, as the server sends it.
func costSeriesFix(id, name string, bars ...map[string]any) map[string]any {
	return map[string]any{"id": id, "name": name, "bars": bars}
}

// costDailyFix is a daily chart as the server sends it: its own bars are the
// DeepSeek bars, and series is the series array the page draws.
func costDailyFix(bars []map[string]any, series ...map[string]any) map[string]any {
	return map[string]any{"bars": bars, "series": series, "aria": "daily spend"}
}

// minBarTop is the highest mark the page drew: the smallest y among the bars.
func minBarTop(bars []costBarGeom) float64 {
	top := math.Inf(1)
	for _, b := range bars {
		top = math.Min(top, b.Y)
	}
	return top
}

// With two series the page draws what it drew before the series array: the same
// marks in the same places, whether they come from the array or from the fields it
// replaces (gt-derxf).
func TestCostBarChartTwoSeriesDrawTheOldShape(t *testing.T) {
	t.Parallel()

	ds := []map[string]any{costBarFix("09:00", 1), costGapFix("10:00")}
	cl := []map[string]any{costBarFix("09:00", 3), costGapFix("10:00")}
	fromSeries := costDailyDrawn(t, costDailyFix(ds,
		costSeriesFix("deepseek", "DeepSeek", ds...),
		costSeriesFix("claude", claudePlanSeriesName, cl...)), nil)
	fromFields := costDailyDrawn(t, map[string]any{
		"bars":   ds,
		"claude": map[string]any{"name": claudePlanSeriesName, "bars": cl},
		"aria":   "daily spend",
	}, nil)

	if !reflect.DeepEqual(fromSeries.Bars, fromFields.Bars) {
		t.Errorf("the series array draws %+v, the fields it replaces drew %+v", fromSeries.Bars, fromFields.Bars)
	}
	if want := []string{"DeepSeek", claudePlanSeriesName}; !reflect.DeepEqual(fromSeries.Legend, want) {
		t.Errorf("legend = %q, want %q", fromSeries.Legend, want)
	}
	var classes []string
	for _, b := range fromSeries.Bars {
		classes = append(classes, b.Class)
	}
	// Bucket one: the DeepSeek bar then the Claude bar. Bucket two: the DeepSeek gap
	// tick then the Claude hatched gap.
	if want := []string{"bar", "cbar", "gap", "cgap"}; !reflect.DeepEqual(classes, want) {
		t.Errorf("marks drawn = %q, want %q", classes, want)
	}
	// The daily amount sits above the first series' one bar; the Claude series has none.
	if want := []string{"$1.00"}; !reflect.DeepEqual(fromSeries.Amounts, want) {
		t.Errorf("daily amounts drawn = %q, want %q", fromSeries.Amounts, want)
	}
	if fromSeries.Patterns != 1 {
		t.Errorf("hatch patterns defined = %d, want the one the hatched gaps fill with", fromSeries.Patterns)
	}
	// Absolute places, from the old pair's geometry: a daily slot of 324px, its
	// wide part 56% of that, halved less a pixel; the DeepSeek bar keeps the left
	// half and the Claude bar starts a pixel right of the slot centre at 188.
	const oldW = (700-2*26)/2.0*0.56/2 - 1
	if math.Abs(fromSeries.Bars[0].X-(188-oldW-1)) > 1e-9 || math.Abs(fromSeries.Bars[0].Width-oldW) > 1e-9 {
		t.Errorf("DeepSeek bar at x=%.2f width %.2f, want x=%.2f width %.2f", fromSeries.Bars[0].X, fromSeries.Bars[0].Width, 188-oldW-1, oldW)
	}
	if math.Abs(fromSeries.Bars[1].X-189) > 1e-9 || math.Abs(fromSeries.Bars[1].Width-oldW) > 1e-9 {
		t.Errorf("Claude bar at x=%.2f width %.2f, want x=189 width %.2f", fromSeries.Bars[1].X, fromSeries.Bars[1].Width, oldW)
	}
}

// A third series draws a third bar in each slot, in its own colour, to the right
// of the two before it, and the legend has an entry for it.
func TestCostBarChartThreeSeriesDrawThreeBarsPerSlot(t *testing.T) {
	t.Parallel()

	ds := []map[string]any{costBarFix("09:00", 1), costBarFix("10:00", 2)}
	cl := []map[string]any{costBarFix("09:00", 3), costBarFix("10:00", 1)}
	ac := []map[string]any{costBarFix("09:00", 1), costBarFix("10:00", 2)}
	got := costDailyDrawn(t, costDailyFix(ds,
		costSeriesFix("deepseek", "DeepSeek", ds...),
		costSeriesFix("claude", claudePlanSeriesName, cl...),
		costSeriesFix("acme", "Acme", ac...)),
		[]any{deepSeekProvider(), claudeProvider(), map[string]any{"id": "acme", "label": "Acme"}})

	if len(got.Bars) != 6 {
		t.Fatalf("marks drawn = %d, want three bars in each of the two slots", len(got.Bars))
	}
	// Each slot holds the series in array order, left to right: the first two wear
	// the classes they always had, the third a bar in the palette colour its
	// provider takes in the legend.
	wantClass := []string{"bar", "cbar", "bar"}
	wantFill := []string{"fill:var(--blue)", "fill:var(--violet)", "fill:var(--red)"}
	for i, b := range got.Bars {
		k := i % 3
		if b.Class != wantClass[k] || b.Style != wantFill[k] {
			t.Errorf("mark %d drew class %q in %q, want class %q in %q", i, b.Class, b.Style, wantClass[k], wantFill[k])
		}
		if k > 0 && b.X <= got.Bars[i-1].X {
			t.Errorf("mark %d sits at x=%.1f, not to the right of the mark before it at x=%.1f", i, b.X, got.Bars[i-1].X)
		}
	}
	if want := []string{"DeepSeek", claudePlanSeriesName, "Acme"}; !reflect.DeepEqual(got.Legend, want) {
		t.Errorf("legend = %q, want one entry per drawn series %q", got.Legend, want)
	}
	if got.Patterns != 1 {
		t.Errorf("hatch patterns defined = %d, want the one the gaps of the later series fill with", got.Patterns)
	}
	// The Claude bar at $3 is the tallest drawn, so it reaches the top of the scale.
	if top := minBarTop(got.Bars); math.Abs(top-16) > 1e-9 {
		t.Errorf("the tallest bar starts at y=%.2f, want the top of the scale at 16", top)
	}
}

// A series with a reason, or with the wrong number of bars, is not drawn: a line
// under the chart names it and why, the other series still draw, and the y scale
// is the tallest bar drawn, not the one left out (gt-derxf).
func TestCostBarChartDropsASeriesNotDrawnAndSaysWhy(t *testing.T) {
	t.Parallel()

	ds := []map[string]any{costBarFix("09:00", 2), costBarFix("10:00", 4)}
	got := costDailyDrawn(t, costDailyFix(ds,
		costSeriesFix("deepseek", "DeepSeek", ds...),
		map[string]any{"id": "claude", "name": claudePlanSeriesName, "why": "the samples could not be read"},
		costSeriesFix("acme", "Acme", costBarFix("09:00", 900))), nil)

	if len(got.Bars) != 2 {
		t.Fatalf("marks drawn = %d, want the DeepSeek bars alone", len(got.Bars))
	}
	for _, b := range got.Bars {
		if b.Class != "bar" {
			t.Errorf("a mark of class %q drawn, want only the DeepSeek bars", b.Class)
		}
	}
	if top := minBarTop(got.Bars); math.Abs(top-16) > 1e-9 {
		t.Errorf("the tallest bar starts at y=%.2f, want 16: the $900 series not drawn must not set the scale", top)
	}
	if want := []string{"DeepSeek"}; !reflect.DeepEqual(got.Legend, want) {
		t.Errorf("legend = %q, want only the drawn series %q", got.Legend, want)
	}
	for _, line := range []string{
		claudePlanSeriesName + " series not drawn: the samples could not be read",
		"Acme series not drawn: the series does not line up with the chart's buckets: it has 1 where the chart has 2",
	} {
		if !strings.Contains(got.Text, line) {
			t.Errorf("the panel does not say %q:\n%s", line, got.Text)
		}
	}
}

// The first series' role (its bar class, gap tick and daily amount) belongs to the
// series at the front of the array. A series left out does not pass it on: the
// drawn series after it keeps the second series' look (gt-xzdna).
func TestCostBarChartRoleFollowsTheArrayNotTheDrawnSeries(t *testing.T) {
	t.Parallel()

	ds := []map[string]any{costBarFix("09:00", 2), costGapFix("10:00")}
	got := costDailyDrawn(t, costDailyFix(ds,
		map[string]any{"id": "claude", "name": claudePlanSeriesName, "why": "the samples could not be read"},
		costSeriesFix("deepseek", "DeepSeek", ds...)), nil)

	var classes []string
	for _, b := range got.Bars {
		classes = append(classes, b.Class)
	}
	// The DeepSeek series is second in the array, so its bar takes the second series'
	// class and its gap is hatched, not the first series' tick.
	if want := []string{"cbar", "cgap"}; !reflect.DeepEqual(classes, want) {
		t.Errorf("marks drawn = %q, want %q", classes, want)
	}
	if len(got.Amounts) != 0 {
		t.Errorf("daily amounts drawn = %q, want none: the first series in the array is not drawn", got.Amounts)
	}
}

// A server older than the series array sends the DeepSeek bars and the Claude
// fields: the page draws them as it always did, and a Claude series it cannot draw
// says why.
func TestCostBarChartWithNoSeriesFieldDrawsBarsAndSaysWhyClaudeIsAbsent(t *testing.T) {
	t.Parallel()

	ds := []map[string]any{costBarFix("09:00", 1), costGapFix("10:00")}
	got := costDailyDrawn(t, map[string]any{
		"bars":       ds,
		"claude_why": "the status line has written no sample yet",
		"aria":       "daily spend",
	}, nil)

	var classes []string
	for _, b := range got.Bars {
		classes = append(classes, b.Class)
	}
	if want := []string{"bar", "gap"}; !reflect.DeepEqual(classes, want) {
		t.Errorf("marks drawn = %q, want the DeepSeek bar and its gap tick %q", classes, want)
	}
	if want := []string{"DeepSeek"}; !reflect.DeepEqual(got.Legend, want) {
		t.Errorf("legend = %q, want %q", got.Legend, want)
	}
	if want := "Claude series not drawn: the status line has written no sample yet"; !strings.Contains(got.Text, want) {
		t.Errorf("the panel does not say %q:\n%s", want, got.Text)
	}
}
