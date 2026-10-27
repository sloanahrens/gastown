package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// headlineTile is one tile of the Cost panel's headline row as drawn: the value
// and the key under it.
type headlineTile struct {
	Value string `json:"value"`
	Key   string `json:"key"`
}

// headlineTiles reads back the headline row of the Cost panel renderCost draws
// from s. The row is found by the class it wears, not by a path through the
// tree, so a part drawn above it does not move it.
func headlineTiles(t *testing.T, s map[string]any) []headlineTile {
	t.Helper()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", jsonValue(t, s))
	v, err := dom.vm.RunString(`JSON.stringify((() => {
		let row = null;
		(function walk(n) {
			if (row || !n) return;
			if (String(n.className || "").split(" ").includes("costheadline")) { row = n; return; }
			for (const c of (n.children || [])) walk(c);
		})(__byId["cost"]);
		if (!row) return null;
		return row.children.map(c => ({value: c.children[0].textContent, key: c.children[1].textContent}));
	})())`)
	if err != nil {
		t.Fatalf("reading the Cost headline: %v", err)
	}
	if v.String() == "null" {
		t.Fatal("the Cost panel drew no headline row")
	}
	var tiles []headlineTile
	if err := json.Unmarshal([]byte(v.String()), &tiles); err != nil {
		t.Fatalf("reading the Cost headline: %v", err)
	}
	return tiles
}

// tileByKey is the tile whose key contains want, failing when the row drew
// none: a row that lost the figure reads as a failure, not as an empty string.
func tileByKey(t *testing.T, tiles []headlineTile, want string) headlineTile {
	t.Helper()

	for _, tile := range tiles {
		if strings.Contains(tile.Key, want) {
			return tile
		}
	}
	t.Fatalf("the headline has no tile labelled %q: %+v", want, tiles)
	return headlineTile{}
}

// costLeafTexts is every leaf string the Cost panel drew, so a test reads a
// line by what it says rather than by where it sits in the tree.
func costLeafTexts(t *testing.T, s map[string]any) []string {
	t.Helper()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", jsonValue(t, s))
	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const out = [];
		(function walk(n) {
			const kids = (n && n.children) || [];
			if (!kids.length) { if (String(n.textContent || "") !== "") out.push(String(n.textContent)); return; }
			for (const c of kids) walk(c);
		})(__byId["cost"]);
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	var texts []string
	if err := json.Unmarshal([]byte(v.String()), &texts); err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	return texts
}

// headlineSpend is a spend report carrying every figure the headline row draws.
func headlineSpend(t *testing.T) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(map[string]any{
		"balance": 77.49, "spent_ledger": 12.34, "ledger_since": "2026-10-19T00:00",
		"pace_1h": map[string]any{"per_hour": 0.20}, "pace_24h": map[string]any{"per_hour": 0.30},
		"runway_days_at_24h_pace": 12.5,
		"live":                    map[string]any{"ok": true, "available": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Every DeepSeek figure in the headline row says so on its own tile, and the
// Claude plan-equivalent dollars sit in the row beside them, marked as the
// estimate they are, so the row no longer reads as if DeepSeek were the town's
// whole spend (gt-iazpv).
func TestCostHeadlineLabelsEveryDeepSeekFigureAndAddsClaude(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)
	charts := BuildCostCharts(claudeSeriesNow, nil, nil, 0, claudeSeriesPlan, claudeHeadlineSamples(start))
	if charts.Headline == nil || charts.Headline.Today == nil || charts.Headline.PerHour24h == nil {
		t.Fatalf("the fixture built no headline to draw: %+v", charts.Headline)
	}
	tiles := headlineTiles(t, map[string]any{
		"spend":       headlineSpend(t),
		"cost_charts": charts,
	})
	if len(tiles) != 6 {
		t.Fatalf("the headline drew %d tiles: %+v, want five DeepSeek figures and one Claude", len(tiles), tiles)
	}

	// Every DeepSeek figure names DeepSeek on its own tile, and each keeps the
	// report's value.
	for _, figure := range []string{"balance", "spent", "last 1h", "last 24h", "runway"} {
		tile := tileByKey(t, tiles, figure)
		if !strings.HasPrefix(tile.Key, "DeepSeek ") {
			t.Errorf("the %s tile is labelled %q, want it to name DeepSeek", figure, tile.Key)
		}
	}
	deep := 0
	for _, tile := range tiles {
		if strings.HasPrefix(tile.Key, "DeepSeek ") {
			deep++
		}
	}
	if deep != 5 {
		t.Errorf("the headline drew %d DeepSeek tiles, want five", deep)
	}
	if got := tileByKey(t, tiles, "balance").Value; got != "$77.49" {
		t.Errorf("the headline balance = %q, want the report's $77.49", got)
	}
	if got := tileByKey(t, tiles, "last 24h").Value; got != "$0.30/h" {
		t.Errorf("the headline last 24h = %q, want the report's $0.30/h", got)
	}

	claude := tileByKey(t, tiles, claudePlanSeriesName)
	if !strings.Contains(claude.Key, claudePlanSeriesName) {
		t.Errorf("the Claude tile is labelled %q, want the legend's %q", claude.Key, claudePlanSeriesName)
	}
	for _, figure := range []string{money(*charts.Headline.Today) + " today", money(*charts.Headline.PerHour24h) + "/h last 24h"} {
		if !strings.Contains(claude.Value, figure) {
			t.Errorf("the Claude tile shows %q, want %q among it", claude.Value, figure)
		}
	}
}

// A rate read over fewer hours than its window says how many it read, so a
// reader weighing it knows how much of the window the series covered (gt-iazpv).
func TestCostHeadlineCountsTheHoursItRead(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)
	var skip []time.Time
	for h := 3; h < 9; h++ {
		skip = append(skip, time.Date(2026, 10, 25, h, 0, 0, 0, time.Local))
	}
	charts := BuildCostCharts(claudeSeriesNow, nil, nil, 0, claudeSeriesPlan, claudeHeadlineSamples(start, skip...))
	tiles := headlineTiles(t, map[string]any{
		"spend":       headlineSpend(t),
		"cost_charts": charts,
	})
	claude := tileByKey(t, tiles, claudePlanSeriesName)
	if !strings.Contains(claude.Key, "18 of 24 hours read") {
		t.Errorf("the Claude tile of a window the series read 18 hours of is labelled %q", claude.Key)
	}
}

// A server that sent no Claude headline leaves the page's own name for the row,
// the legend's: a change to one that left the other behind would name the tile
// a series the charts no longer carry, and the row still says why it has no
// figure rather than drawing a zero (gt-iazpv).
func TestCostHeadlineNamesItselfWithTheLegendWhenTheServerSendsNone(t *testing.T) {
	t.Parallel()

	tiles := headlineTiles(t, map[string]any{
		"spend":       headlineSpend(t),
		"cost_charts": map[string]any{},
	})
	claude := tileByKey(t, tiles, claudePlanSeriesName)
	if claude.Value != "not available" {
		t.Errorf("the tile of a headline the server did not send = %q, want it to say it is not available", claude.Value)
	}
}

// A Claude series the server could not build says why on the headline's tile,
// so the row never draws the $0.00 of a plan nobody used (gt-iazpv).
func TestCostHeadlineSaysWhyTheClaudeSeriesIsMissing(t *testing.T) {
	t.Parallel()

	charts := BuildCostCharts(claudeSeriesNow, nil, nil, 0, claudeSeriesPlan, PlanSamples{Read: true, Failed: true})
	if charts.Headline == nil || charts.Headline.Why == "" {
		t.Fatalf("the fixture built no reason to draw: %+v", charts.Headline)
	}
	tiles := headlineTiles(t, map[string]any{
		"spend":       headlineSpend(t),
		"cost_charts": charts,
	})
	claude := tileByKey(t, tiles, charts.Headline.Why)
	if !strings.Contains(claude.Key, charts.Headline.Why) {
		t.Errorf("the Claude tile is labelled %q, want it to say %q", claude.Key, charts.Headline.Why)
	}
	if strings.Contains(claude.Value, "$") || strings.Contains(claude.Key, "$") {
		t.Errorf("a Claude series it could not build drew a dollar figure: %q over %q", claude.Value, claude.Key)
	}
}

// The cash and credit the balance splits into are DeepSeek's too, so the line
// under the balance says whose they are like the tiles above it (gt-iazpv).
func TestCostHeadlineCashAndCreditAreDeepSeek(t *testing.T) {
	t.Parallel()

	texts := costLeafTexts(t, map[string]any{"spend": map[string]any{
		"balance": 15.0, "live": map[string]any{"ok": true},
		"cash": 10.0, "credit": 5.0, "credit_runway_days_at_24h_pace": 2.0,
	}})
	want := "DeepSeek $10.00 cash + $5.00 credit · credit lasts ~2.0d, then cash"
	for _, text := range texts {
		if text == want {
			return
		}
	}
	t.Errorf("the cash and credit line is not among %q, want %q", texts, want)
}

// The Claude row is not the spend report's: it draws without one, so a reader
// is not left without the plan's spend when DeepSeek's report is missing
// (gt-iazpv).
func TestCostHeadlineDrawsClaudeWithoutASpendReport(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)
	charts := BuildCostCharts(claudeSeriesNow, nil, nil, 0, claudeSeriesPlan, claudeHeadlineSamples(start))
	if charts.Headline == nil || charts.Headline.Today == nil {
		t.Fatalf("the fixture built no headline to draw: %+v", charts.Headline)
	}
	today := money(*charts.Headline.Today) + " today"

	// Usage answered, so the panel draws its parts; and nothing else read, so
	// it takes its one-line empty state. Neither has a spend report.
	cases := map[string]map[string]any{
		"usage answered":    {"usage": map[string]any{}, "cost_charts": charts},
		"nothing else read": {"cost_charts": charts},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tiles := headlineTiles(t, s)
			claude := tileByKey(t, tiles, claudePlanSeriesName)
			if !strings.Contains(claude.Value, today) {
				t.Errorf("the Claude tile without a spend report = %q, want it to carry %q", claude.Value, today)
			}
		})
	}
}

// The panel caption names each of its halves: the est. figure is DeepSeek's
// usage-record estimate and the over-20m count is beads, so neither half reads
// as the other's (gt-iazpv).
func TestCostPanelCaptionNamesItsEstimateAndItsBeads(t *testing.T) {
	t.Parallel()

	usage := map[string]any{
		"as_of": time.Now().UTC().Format(time.RFC3339),
		"by_class_model": []any{
			map[string]any{"class": "interactive", "model": "deepseek-chat", "calls": 4, "est_cost_usd": 0.42},
		},
		"over20": []any{
			map[string]any{"bead": "gt-1"}, map[string]any{"bead": "gt-2"},
		},
	}
	_, note := drawCost(t, jsonValue(t, map[string]any{"spend": headlineSpend(t), "usage": usage}))
	for _, want := range []string{"DeepSeek est. $0.42", "2 beads over 20m"} {
		if !strings.Contains(note.Text, want) {
			t.Errorf("the caption = %q, want %q among it", note.Text, want)
		}
	}
	if !strings.Contains(note.Title, "DeepSeek") {
		t.Errorf("the caption's tooltip = %q, want it to name the DeepSeek estimate", note.Title)
	}

	// One slow bead is a bead, not a beads.
	usage["over20"] = []any{map[string]any{"bead": "gt-1"}}
	_, note = drawCost(t, jsonValue(t, map[string]any{"spend": headlineSpend(t), "usage": usage}))
	if !strings.Contains(note.Text, "1 bead over 20m") {
		t.Errorf("the caption with one slow bead = %q", note.Text)
	}
}
