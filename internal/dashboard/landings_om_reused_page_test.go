package dashboard

import (
	"encoding/json"
	"testing"
	"time"
)

// omCell is one landing's om cell as the Landings table draws it: its text and
// its class.
type omCell struct {
	Text  string
	Class string
}

// omCellsByBead reads every drawn landings row's om cell (the tenth cell) back
// by the row's bead id, so a test reads the drawn page rather than the source
// that draws it (gt-qr62xp).
func omCellsByBead(t *testing.T, p *pageDOM) map[string]omCell {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var tbl = __find(document.getElementById("trend"), ".ltable");
  if (!tbl) return [];
  function part(e) { return e ? e.textContent : ""; }
  return tbl.children.__arr.slice(1).map(function (tr) {
    var om = tr.children[9];
    return {id: part(__find(tr.children[3], ".bead")), text: part(om), class: om.className};
  });
})())`)
	if err != nil {
		t.Fatalf("reading the drawn om cells: %v", err)
	}
	var rows []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
		Cls  string `json:"class"`
	}
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the drawn om cells: %v", err)
	}
	cells := make(map[string]omCell, len(rows))
	for _, r := range rows {
		cells[r.ID] = omCell{Text: r.Text, Class: r.Cls}
	}
	return cells
}

// A landed row's om cell reads the review's time followed by "reused" when the
// landing reused a stored review (gt-qr62xp). A reused row with no time reads
// "reused" alone, a run row reads its time alone, and a landing with no review
// reads the dash. A reused review over 240 s keeps the warn class a run has.
func TestLandingsOMCellReadsAReusedReviewsTime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	landed := func(bead string, m int, extra map[string]any) map[string]any {
		row := map[string]any{
			"at":   now.Add(-time.Duration(m) * time.Minute).Format(time.RFC3339),
			"bead": bead, "rig": "gastown", "outcome": "landed", "polecat": "dag",
		}
		for k, v := range extra {
			row[k] = v
		}
		return row
	}
	reading := map[string]any{"trend": map[string]any{
		"hours":  []map[string]any{{"hour": now.Add(-time.Hour).Format(time.RFC3339), "landed": 5, "rejected": 0}},
		"stages": []any{},
		"recent": []any{
			landed("gt-reused-timed", 40, map[string]any{"om_secs": 38.0, "om_reused": true}),
			landed("gt-reused-bare", 30, map[string]any{"om_reused": true}),
			landed("gt-run", 20, map[string]any{"om_secs": 61.0}),
			landed("gt-reused-slow", 15, map[string]any{"om_secs": 300.0, "om_reused": true}),
			landed("gt-no-review", 10, nil),
		},
	}}

	p := newPageDOM(t)
	p.call(t, "renderTrend", reading)

	want := map[string]omCell{
		"gt-reused-timed": {Text: "38s reused", Class: "n"},
		"gt-reused-bare":  {Text: "reused", Class: "n"},
		"gt-run":          {Text: "61s", Class: "n"},
		"gt-reused-slow":  {Text: "5m reused", Class: "n warnc"},
		"gt-no-review":    {Text: "–", Class: "n"},
	}
	got := omCellsByBead(t, p)
	for bead, w := range want {
		g, ok := got[bead]
		if !ok {
			t.Errorf("the table drew no row for %s", bead)
			continue
		}
		if g.Text != w.Text {
			t.Errorf("the om cell of %s reads %q, want %q", bead, g.Text, w.Text)
		}
		if g.Class != w.Class {
			t.Errorf("the om cell of %s has class %q, want %q", bead, g.Class, w.Class)
		}
	}
}
