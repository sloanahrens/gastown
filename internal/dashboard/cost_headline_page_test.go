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
