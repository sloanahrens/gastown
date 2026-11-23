package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

// The trend draws the daily estimate against actual under its hourly bars: one
// row per landing day with the landings counted, the median actual over
// estimated minutes and the share within 25 percent of the estimate, and "too
// few" for a day too thin to judge (gt-1xtwib).
func TestIndexEstimateDayRows(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", map[string]any{"trend": map[string]any{
		"hours":  []map[string]any{{"hour": "2026-11-22T15:00:00Z", "landed": 10, "rejected": 0}},
		"stages": []any{},
		"recent": []any{},
		"estimate_days": []map[string]any{
			{"day": "2026-11-21", "n": 6, "skipped": 2, "median_ratio": 1.25, "within_25": 0.5},
			{"day": "2026-11-22", "n": 4, "skipped": 0, "median_ratio": 0, "within_25": 0},
		},
	}})

	var rows []struct {
		Text  string `json:"text"`
		Title string `json:"title"`
	}
	got, err := p.vm.RunString(`JSON.stringify((function () {
  var box = __find(document.getElementById("trend"), ".estdays");
  if (!box) return [];
  return box.children.__arr.filter(function (c) { return __cls(c, "erow"); }).map(function (r) {
    return {text: r.textContent, title: r.title};
  });
})())`)
	if err != nil {
		t.Fatalf("reading the estimate day rows: %v", err)
	}
	if err := json.Unmarshal([]byte(got.String()), &rows); err != nil {
		t.Fatalf("reading the estimate day rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("estimate day rows = %d (%+v), want one per day", len(rows), rows)
	}
	for _, want := range []string{"2026-11-21", "6 landings", "1.25", "50% within 25% of the estimate"} {
		if !strings.Contains(rows[0].Text, want) {
			t.Errorf("the judged day's row %q has no %q", rows[0].Text, want)
		}
	}
	if !strings.Contains(rows[0].Title, "2 landings skipped") {
		t.Errorf("the judged day's row title = %q, want its skipped count", rows[0].Title)
	}
	for _, want := range []string{"2026-11-22", "4 landings", "too few"} {
		if !strings.Contains(rows[1].Text, want) {
			t.Errorf("the thin day's row %q has no %q", rows[1].Text, want)
		}
	}
	if strings.Contains(rows[1].Text, "median") {
		t.Errorf("the thin day draws a ratio instead of too few: %q", rows[1].Text)
	}
}

// A trend with no landing day, and a feed written before the field existed,
// draw no estimate block at all rather than an empty or zeroed one (gt-1xtwib).
func TestIndexEstimateDayRowsDrawNothingWithoutDays(t *testing.T) {
	t.Parallel()

	for _, days := range []any{[]any{}, nil} {
		trend := map[string]any{
			"hours":  []map[string]any{{"hour": "2026-11-22T15:00:00Z", "landed": 1}},
			"stages": []any{},
			"recent": []any{},
		}
		if days != nil {
			trend["estimate_days"] = days
		}
		p := newPageDOM(t)
		p.call(t, "renderTrend", map[string]any{"trend": trend})
		drawn, err := p.vm.RunString(`__find(document.getElementById("trend"), ".estdays") === null`)
		if err != nil {
			t.Fatalf("reading the estimate day block for %v: %v", days, err)
		}
		if !drawn.ToBoolean() {
			t.Errorf("a trend with %v draws an estimate day block, want none", days)
		}
	}
}
