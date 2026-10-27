package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// headlineTile is one tile of the Cost panel's headline row as drawn: the value,
// the key under it, and the value's class.
type headlineTile struct {
	Value string `json:"value"`
	Key   string `json:"key"`
	Class string `json:"class"`
}

// headlineTiles reads back the headline row of the Cost panel renderCost draws
// from s.
func headlineTiles(t *testing.T, s map[string]any) []headlineTile {
	t.Helper()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", jsonValue(t, s))
	v, err := dom.vm.RunString(`JSON.stringify(__byId["cost"].children[0].children[0].children.map(c => ({value: c.children[0].textContent, key: c.children[1].textContent, class: c.children[0].className})))`)
	if err != nil {
		t.Fatalf("reading the Cost headline: %v", err)
	}
	var tiles []headlineTile
	if err := json.Unmarshal([]byte(v.String()), &tiles); err != nil {
		t.Fatalf("reading the Cost headline: %v", err)
	}
	return tiles
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

	deep := tiles[:len(tiles)-1]
	want := []string{"balance", "spent", "last 1h", "last 24h", "runway"}
	for i, tile := range deep {
		if !strings.HasPrefix(tile.Key, "DeepSeek ") || !strings.Contains(tile.Key, want[i]) {
			t.Errorf("DeepSeek tile %d is labelled %q, want it to name DeepSeek and %q", i, tile.Key, want[i])
		}
	}
	// The figures are the report's own, still, under the new key.
	if deep[0].Value != "$77.49" || deep[3].Value != "$0.30/h" {
		t.Errorf("the headline shows %q balance and %q last 24h, want the report's $77.49 and $0.30/h", deep[0].Value, deep[3].Value)
	}

	claude := tiles[len(tiles)-1]
	if !strings.Contains(claude.Key, claudePlanSeriesName) {
		t.Errorf("the Claude tile is labelled %q, want the legend's %q", claude.Key, claudePlanSeriesName)
	}
	for _, figure := range []string{money(*charts.Headline.Today) + " today", money(*charts.Headline.PerHour24h) + "/h last 24h"} {
		if !strings.Contains(claude.Value, figure) {
			t.Errorf("the Claude tile shows %q, want %q among it", claude.Value, figure)
		}
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
	claude := tiles[len(tiles)-1]
	if !strings.Contains(claude.Key, charts.Headline.Why) {
		t.Errorf("the Claude tile is labelled %q, want it to say %q", claude.Key, charts.Headline.Why)
	}
	if strings.Contains(claude.Value, "$") || strings.Contains(claude.Key, "$") {
		t.Errorf("a Claude series it could not build drew a dollar figure: %q over %q", claude.Value, claude.Key)
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
