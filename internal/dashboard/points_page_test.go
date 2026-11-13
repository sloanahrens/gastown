package dashboard

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Polecats table has a Points column that reads a seat's bead estimate then
// how long the bead has been dispatched, and the Landings table has one that
// reads a landing's estimate then its measured points. The page draws only what
// the feed sends: no Fibonacci scale or minutes-to-points conversion is in the
// page, so the points are the feed's own figures (gt-drlxcn).

// pointsCell is one landings row's Points cell as the page drew it, with the
// bead the row draws and the attribute the stacked layout labels it by.
type pointsCell struct {
	Bead  string `json:"bead"`
	Text  string `json:"text"`
	Title string `json:"title"`
	Label string `json:"label"`
	Cells int    `json:"cells"`
}

// landingPointsCells reads the Points cell of every drawn landings row, in the
// order the table drew them.
func landingPointsCells(t *testing.T, p *pageDOM) []pointsCell {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var tbl = __find(document.getElementById("trend"), ".ltable");
  if (!tbl) return [];
  return tbl.children.__arr.slice(1).map(function (tr) {
    var pts = tr.children[6];
    return {bead: __find(tr.children[3], ".bead").textContent, text: pts.textContent,
            title: pts.title || "", label: pts.getAttribute("data-label") || "", cells: tr.children.length};
  });
})())`)
	if err != nil {
		t.Fatalf("reading the drawn Points cells: %v", err)
	}
	var out []pointsCell
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the drawn Points cells: %v", err)
	}
	return out
}

// pointsByBead keys the drawn Points readings by the bead each row draws.
func pointsByBead(t *testing.T, p *pageDOM) map[string]string {
	t.Helper()

	out := map[string]string{}
	for _, c := range landingPointsCells(t, p) {
		out[c.Bead] = c.Text
	}
	return out
}

// TestALandingRowReadsItsEstimateThenItsActualPoints draws four finished
// landings, one per side that can be absent, and reads each Points cell back:
// both sides read estimate → actual, a missing side reads a dash in its place,
// and a row with neither reads one dash.
func TestALandingRowReadsItsEstimateThenItsActualPoints(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 16, 0, 0, 0, time.Local)
	landed := func(bead string, est, actual any) map[string]any {
		row := map[string]any{
			"at": now.Add(-5 * time.Minute).Format(time.RFC3339), "bead": bead, "rig": "gastown",
			"outcome": "landed", "polecat": "dag", "priority": 2, "title": "a landed bead",
		}
		if est != nil {
			row["sp_estimate"] = est
		}
		if actual != nil {
			row["sp_actual"] = actual
		}
		return row
	}
	p := newPageDOM(t)
	p.call(t, "renderTrend", map[string]any{"trend": map[string]any{
		"hours":  []map[string]any{{"hour": now.Add(-time.Hour).Format(time.RFC3339), "landed": 4, "rejected": 0}},
		"stages": []any{},
		"recent": []any{
			landed("gt-both", 5, 8),
			landed("gt-actual", nil, 8),
			landed("gt-estimate", 5, nil),
			landed("gt-neither", nil, nil),
		},
	}})

	got := pointsByBead(t, p)
	for bead, want := range map[string]string{
		"gt-both":     "5 → 8",
		"gt-actual":   "– → 8",
		"gt-estimate": "5 → –",
		"gt-neither":  "–",
	} {
		if got[bead] != want {
			t.Errorf("the %s row's Points cell reads %q, want %q", bead, got[bead], want)
		}
	}
}

// TestAQueuedAndRunningRowReadItsEstimateThenADash draws a bead waiting to land
// and a landing running now, each with an estimate and no measured points yet:
// both read the estimate, then a dash for the actual they have not earned.
func TestAQueuedAndRunningRowReadItsEstimateThenADash(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", map[string]any{
		"queue": map[string]any{"landing": []any{map[string]any{
			"id": "gt-queued", "rig": "gastown", "title": "a queued bead", "priority": 1,
			"assignee": "gastown/polecats/dag", "updated_at": now.Add(-20 * time.Minute).Format(time.RFC3339),
			"sp_estimate": 5,
		}}},
		"trend": map[string]any{
			"hours":  []map[string]any{},
			"stages": []any{},
			"recent": []any{map[string]any{
				"at": now.Add(-15 * time.Minute).Format(time.RFC3339), "bead": "gt-running", "rig": "gastown",
				"outcome": "running", "polecat": "dag", "sp_estimate": 3,
			}},
		},
	})

	got := pointsByBead(t, p)
	if got["gt-queued"] != "5 → –" {
		t.Errorf("the queued row's Points cell reads %q, want %q", got["gt-queued"], "5 → –")
	}
	if got["gt-running"] != "3 → –" {
		t.Errorf("the running row's Points cell reads %q, want %q", got["gt-running"], "3 → –")
	}
}

// TestTheSeatPointsColumnReadsEstimateThenElapsed draws four seats: one with an
// estimate and a dispatch time, one with no estimate, one with no dispatch time,
// and an idle one holding nothing. The Points cell reads the estimate then the
// elapsed time, a dash for whichever is absent, and an empty cell for the seat
// holding no bead.
func TestTheSeatPointsColumnReadsEstimateThenElapsed(t *testing.T) {
	t.Parallel()

	alpha := seat("gastown", "alpha", "working", "gt-alpha", "a seat with an estimate", "deepseek-flash", 12*time.Minute)
	alpha["sp_estimate"] = 5
	bravo := seat("gastown", "bravo", "working", "gt-bravo", "a seat with no estimate", "deepseek-flash", 3*time.Minute)
	charlie := seat("gastown", "charlie", "working", "gt-charlie", "a seat with no dispatch time", "deepseek-flash", 0)
	charlie["sp_estimate"] = 8
	delta := seat("gastown", "delta", "idle", "", "", "", 0)

	dom := newPageDOM(t)
	callWork(t, dom, workState([]any{alpha, bravo, charlie, delta}, workQueueReading(1, 0, 0, 0)))

	cells := workCells(t, dom, "wpts")
	want := []string{"5 · 12m", "– · 3m", "8 · –", ""}
	if len(cells) != len(want) {
		t.Fatalf("the panel drew %d Points cells, want %d", len(cells), len(want))
	}
	for i, w := range want {
		if cells[i].Text != w {
			t.Errorf("seat %d's Points cell reads %q, want %q", i, cells[i].Text, w)
		}
	}
}

// TestTheSeatPointsColumnMovesWithTheClock redraws an unchanged reading ten
// minutes later: the elapsed part of the Points cell moves with the clock, and
// the row is the same element, so no feed change is needed to move it.
func TestTheSeatPointsColumnMovesWithTheClock(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	p := seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", 12*time.Minute)
	p["sp_estimate"] = 5
	p["slung"], p["last_active"] = base.Add(-12*time.Minute).Format(time.RFC3339), base.Add(-time.Minute).Format(time.RFC3339)
	st := workState([]any{p}, workQueueReading(1, 0, 0, 0))
	st["summary_at"] = base.Format(time.RFC3339)
	for _, r := range st["reads"].(map[string]any) {
		r.(map[string]any)["at"] = base.Format(time.RFC3339)
	}

	dom := newPageDOM(t)
	workClock(t, dom, base)
	callWork(t, dom, st)
	if _, err := dom.vm.RunString(`globalThis.__seatRow = __byId["polecats"].children[0].children[1]`); err != nil {
		t.Fatal(err)
	}
	if got := workCells(t, dom, "wpts"); len(got) != 1 || got[0].Text != "5 · 12m" {
		t.Fatalf("the Points cell reads %+v before the clock moves, want 5 · 12m", got)
	}

	workClock(t, dom, base.Add(10*time.Minute))
	callWork(t, dom, st)
	if got := workCells(t, dom, "wpts"); len(got) != 1 || got[0].Text != "5 · 22m" {
		t.Errorf("the Points cell reads %+v ten minutes on, want 5 · 22m", got)
	}
	same, err := dom.vm.RunString(`String(__byId["polecats"].children[0].children[1] === globalThis.__seatRow)`)
	if err != nil {
		t.Fatal(err)
	}
	if same.String() != "true" {
		t.Error("an unchanged reading rebuilt the seat's row instead of moving its Points cell in place")
	}
}

// TestThePointsHeadersSayWhatEachFigureIs reads the two headers: each Points
// heading carries a hover that says what its figures are, and the Landings
// hover names the Fibonacci rounding the actual is drawn with.
func TestThePointsHeadersSayWhatEachFigureIs(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	callWork(t, dom, workState([]any{
		seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", time.Minute),
	}, workQueueReading(1, 0, 0, 0)))
	seatHead := workHeadCell(t, dom, `__byId["polecats"].children.__arr.find(c => String(c.className).split(" ").indexOf("wtable") >= 0)`, 5)
	if seatHead.Text != "Points" || !strings.Contains(seatHead.Title, "story-point estimate") || !strings.Contains(seatHead.Title, "not points") {
		t.Errorf("the Polecats Points heading reads %q (%q), want Points with a hover that says what each figure is", seatHead.Text, seatHead.Title)
	}

	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsOutcomes(time.Date(2026, 11, 13, 16, 0, 0, 0, time.Local)))
	landHead := workHeadCell(t, p, `__find(document.getElementById("trend"), ".ltable")`, 6)
	if landHead.Text != "Points" || !strings.Contains(landHead.Title, "estimate → actual") || !strings.Contains(landHead.Title, "rounded to the nearest Fibonacci number") {
		t.Errorf("the Landings Points heading reads %q (%q), want Points with the estimate, the actual and its rounding on hover", landHead.Text, landHead.Title)
	}
}

// workHeadCell reads one header cell of a table: table is a JavaScript
// expression that evaluates to the table, and index is the column.
func workHeadCell(t *testing.T, p *pageDOM, table string, index int) drawn {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((() => {
		const t = ` + table + `;
		const c = t && t.children[0].children.__arr[` + strconv.Itoa(index) + `];
		return c ? {text: c.textContent, title: c.title || ""} : null;
	})())`)
	if err != nil {
		t.Fatalf("reading the header cell %d: %v", index, err)
	}
	if v.String() == "null" {
		t.Fatalf("the table drew no header cell %d", index)
	}
	var d drawn
	if err := json.Unmarshal([]byte(v.String()), &d); err != nil {
		t.Fatalf("reading the header cell %d: %v", index, err)
	}
	return d
}

// TestTheLandingsPointsCellIsLabelledForTheStackedLayout reads each landings
// row's Points cell back: the stacked layout below 640 px labels every cell by
// its column's heading, so the Points cell must carry the Points label.
func TestTheLandingsPointsCellIsLabelledForTheStackedLayout(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsOutcomes(now))

	cells := landingPointsCells(t, p)
	if len(cells) == 0 {
		t.Fatal("the landings table drew no rows")
	}
	for _, c := range cells {
		if c.Label != "Points" {
			t.Errorf("the %s row's Points cell is labelled %q, want Points", c.Bead, c.Label)
		}
	}
}

// fibonacciRun matches three consecutive Fibonacci numbers in order, the shape a
// points scale takes when it is written into the page.
var fibonacciRun = regexp.MustCompile(`\b(1|2|3|5|8|13|21|34|55|89)\b\s*,\s*\b(2|3|5|8|13|21|34|55|89)\b\s*,\s*\b(3|5|8|13|21|34|55|89)\b`)

// TestThePageCarriesNoPointsScaleOrConversion reads the page's script and fails
// on a Fibonacci run, the scale the feed's points are rounded onto. The page
// draws the points the feed sends; a copy of the scale would let it convert
// minutes itself (gt-drlxcn).
func TestThePageCarriesNoPointsScaleOrConversion(t *testing.T) {
	t.Parallel()

	script, err := pageScript()
	if err != nil {
		t.Fatal(err)
	}
	if m := fibonacciRun.FindString(script); m != "" {
		t.Errorf("index.html writes the Fibonacci run %q, so the page carries a points scale", m)
	}
}
