package dashboard

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// The Landings table wears a bead's refusal count in its State cell: "rejected
// ×N" on a queued, running or rejected row, and "landed · after N rejections"
// on a landed one, warned from the second refusal on. These read the drawn
// page back, so the pill's text, its warn colour and the cell it sits in fail
// here rather than in a browser (gt-x8oh0q).

// drawnPill is one pill inside a drawn landings State cell.
type drawnPill struct {
	Class string `json:"class"`
	Text  string `json:"text"`
	Title string `json:"title"`
}

// landingsRejectState is a Landings reading whose table holds one row: a queued
// bead when outcome is "queued", else one recent landing of that outcome
// carrying the rejection count n the reader would send.
func landingsRejectState(now time.Time, outcome string, n int) map[string]any {
	at := func(m int) string { return now.Add(-time.Duration(m) * time.Minute).Format(time.RFC3339) }
	hours := []map[string]any{{"hour": now.Add(-time.Hour).Format(time.RFC3339), "landed": 1, "rejected": 0}}
	if outcome == "queued" {
		return map[string]any{
			"queue": map[string]any{"landing": []any{map[string]any{
				"id": "gt-queued", "rig": "gastown", "title": "a bead waiting to land", "priority": 1,
				"assignee": "gastown/polecats/dag", "updated_at": at(20), "reject_count": float64(n),
			}}},
			"trend": map[string]any{"hours": hours, "stages": []any{}, "recent": []any{}},
		}
	}
	row := map[string]any{
		"at": at(10), "bead": "gt-" + outcome, "rig": "gastown", "outcome": outcome,
		"polecat": "dag", "priority": 2, "title": "a " + outcome + " bead", "reject_count": float64(n),
	}
	if outcome == "rejected" {
		row["kind"], row["detail"] = "gate", "the gate failed: TestTheThing"
	}
	if outcome == "landed" {
		row["verdict"] = "approved"
	}
	return map[string]any{
		"queue": map[string]any{"landing": []any{}},
		"trend": map[string]any{"hours": hours, "stages": []any{}, "recent": []any{row}},
	}
}

// landingsStatePills reads every drawn landings row back by its bead id, with
// the pills its State cell (the fifth cell) holds in order.
func landingsStatePills(t *testing.T, p *pageDOM) map[string][]drawnPill {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var tbl = __find(document.getElementById("trend"), ".ltable");
  if (!tbl) return {};
  var out = {};
  tbl.children.__arr.slice(1).forEach(function (tr) {
    var bead = __find(tr.children[3], ".bead");
    out[bead ? bead.textContent : ""] = tr.children[4].children.__arr.map(function (g) {
      return {class: g.className || "", text: g.textContent, title: g.title || ""};
    });
  });
  return out;
})())`)
	if err != nil {
		t.Fatalf("reading the drawn landings State cells: %v", err)
	}
	var rows map[string][]drawnPill
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the drawn landings State cells: %v", err)
	}
	return rows
}

// pillTexts names each pill's text in order.
func pillTexts(pills []drawnPill) []string {
	out := make([]string, len(pills))
	for i, p := range pills {
		out[i] = p.Text
	}
	return out
}

// pillWarns reports whether any pill is drawn in the warn colour.
func pillWarns(pills []drawnPill) bool {
	for _, p := range pills {
		if slices.Contains(strings.Fields(p.Class), "warnc") {
			return true
		}
	}
	return false
}

// A queued Landings row wears its bead's refusal count: a bead refused once
// reads "rejected ×1" beside its queued pill, and a bead with no refusal (a 0
// count, or a reading that sends none) draws the queued pill alone.
func TestLandingsRejectPillQueuedRow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	cases := []struct {
		name  string
		count int
		want  []string
	}{
		{"one refusal draws the count", 1, []string{"queued", "rejected ×1"}},
		{"no refusal draws nothing", 0, []string{"queued"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPageDOM(t)
			p.call(t, "renderTrend", landingsRejectState(now, "queued", c.count))
			pills := landingsStatePills(t, p)["gt-queued"]
			if got := pillTexts(pills); !slices.Equal(got, c.want) {
				t.Fatalf("the queued row's State cell reads %q, want %q", got, c.want)
			}
			if c.count > 0 {
				last := pills[len(pills)-1]
				if !strings.Contains(last.Title, "rejection") {
					t.Errorf("the refusal pill's hover reads %q, want it to name the refusals", last.Title)
				}
				if pillWarns(pills) {
					t.Errorf("a first refusal warns; want the ordinary pill colour until the second")
				}
			}
		})
	}
}

// A running, rejected or landed row wears the same count: "rejected ×N" beside
// the state pill, warned from the second refusal on, and a landed row reads
// where it ended, "landed · after N rejections".
func TestLandingsRejectPillOnEveryOutcome(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	cases := []struct {
		name    string
		outcome string
		count   int
		want    []string
		warned  bool
	}{
		{"a running retry counts its bead's refusals", "running", 1, []string{"running", "rejected ×1"}, false},
		{"a rejected row counts its own last refusal and warns at two", "rejected", 2, []string{"rejected · gate", "rejected ×2"}, true},
		{"a landed row says it landed after them", "landed", 2, []string{"landed · after 2 rejections"}, true},
		{"one refusal before a landing does not warn", "landed", 1, []string{"landed · after 1 rejection"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPageDOM(t)
			p.call(t, "renderTrend", landingsRejectState(now, c.outcome, c.count))
			pills := landingsStatePills(t, p)["gt-"+c.outcome]
			if got := pillTexts(pills); !slices.Equal(got, c.want) {
				t.Fatalf("the %s row's State cell reads %q, want %q", c.outcome, got, c.want)
			}
			if got := pillWarns(pills); got != c.warned {
				t.Errorf("the %s row's warn colour = %v, want %v; State cell %q", c.outcome, got, c.warned, pills)
			}
		})
	}
}

// A landed row whose State cell already wears a pill keeps that pill's word for
// the outcome, so the cell reads "no review" then "after 2 rejections" rather
// than saying landed twice.
func TestLandingsRejectPillOnALandedRowWithoutReview(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	st := landingsRejectState(now, "landed", 2)
	st["trend"].(map[string]any)["recent"].([]any)[0].(map[string]any)["verdict"] = "skipped"
	p := newPageDOM(t)
	p.call(t, "renderTrend", st)

	pills := landingsStatePills(t, p)["gt-landed"]
	want := []string{"no review", "after 2 rejections"}
	if got := pillTexts(pills); !slices.Equal(got, want) {
		t.Errorf("the landed row without a review reads %q, want %q", got, want)
	}
}

// The narrow layout keeps its columns: the pill rides inside the State cell
// every width already draws — the cell that carries the column's heading when
// the row stacks — so a narrow viewport gains no column and no cell.
func TestLandingsRejectPillKeepsTheNarrowLayoutsColumns(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	st := landingsRejectState(now, "rejected", 2)
	st["queue"] = map[string]any{"landing": []any{map[string]any{
		"id": "gt-queued", "rig": "gastown", "title": "a bead waiting to land", "priority": 1,
		"assignee": "gastown/polecats/dag", "updated_at": now.Add(-20 * time.Minute).Format(time.RFC3339),
		"reject_count": 1.0,
	}}}
	p := newPageDOM(t)
	p.call(t, "renderTrend", st)

	head := landingsHeaderCells(t, p)
	if head != 11 {
		t.Fatalf("the table heads %d columns, want the eleven the Landings table draws", head)
	}
	rows := landingsRows(t, p)
	if len(rows) != 2 {
		t.Fatalf("the table drew %d rows, want the queued bead and the rejected landing", len(rows))
	}
	for _, row := range rows {
		if len(row.Cells) != head {
			t.Errorf("the %q row drew %d cells, want the %d columns the table heads", row.Cells[3].Text, len(row.Cells), head)
		}
		for i, c := range row.Cells {
			if c.Span != 1 {
				t.Errorf("the %q row's cell %d spans %d columns, want 1", row.Cells[3].Text, i, c.Span)
			}
		}
		if !strings.Contains(row.Cells[4].Text, "rejected ×") {
			t.Errorf("the %q row's State cell reads %q, want the refusal pill inside it", row.Cells[3].Text, row.Cells[4].Text)
		}
	}
}
