package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The Landings pane draws every finished landing it is given, inside a scroll
// container whose header row stays fixed (gt-1kttu). These render the pane in the
// page harness and count what it drew.
func TestLandingsPaneDrawsEveryLandingInAScrollBox(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 25, 16, 0, 0, 0, time.Local)
	var recent []map[string]any
	for i := 0; i < 30; i++ {
		recent = append(recent, map[string]any{
			"at":      now.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339),
			"bead":    fmt.Sprintf("gt-%d", i),
			"rig":     "gastown",
			"outcome": "landed",
		})
	}
	state := map[string]any{"trend": map[string]any{
		"hours":  []map[string]any{{"hour": now.Truncate(time.Hour).Format(time.RFC3339), "landed": 30, "rejected": 0}},
		"stages": []any{},
		"recent": recent,
	}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}

	p := newPageDOM(t)
	p.call(t, "renderTrend", st)
	counted, err := p.vm.RunString(`(function () {
  var out = {rows: 0, headers: 0, scroll: 0};
  function walk(n, inScroll) {
    if (!n || !n.children) return;
    for (var i = 0; i < n.children.length; i++) {
      var c = n.children[i];
      var cls = c.attr_class || c.className || "";
      var here = inScroll || cls === "lrows";
      if (cls === "lrows") out.scroll++;
      if (here && c.tagName === "TR" && c.children && c.children.length && c.children[0].tagName === "TH") out.headers++;
      if (here && c.tagName === "TR" && c.children && c.children.length && c.children[0].tagName !== "TH") out.rows++;
      walk(c, here);
    }
  }
  walk(document.getElementById("trend"), false);
  return JSON.stringify(out);
})()`)
	if err != nil {
		t.Fatalf("reading the drawn landings pane: %v", err)
	}
	var got struct {
		Rows    int `json:"rows"`
		Headers int `json:"headers"`
		Scroll  int `json:"scroll"`
	}
	if err := json.Unmarshal([]byte(counted.String()), &got); err != nil {
		t.Fatalf("decoding the count: %v", err)
	}
	if got.Scroll != 1 {
		t.Errorf("scroll containers = %d, want the one the landings table sits in", got.Scroll)
	}
	if got.Rows != len(recent) {
		t.Errorf("landing rows drawn = %d, want all %d", got.Rows, len(recent))
	}
	if got.Headers != 1 {
		t.Errorf("header rows drawn in the scroll box = %d, want 1", got.Headers)
	}
}

// Each poll rebuilds the landings rows in a scroll box that opens at the top; the
// redraw must keep the reader's place (gt-tjlu6).
func TestLandingsPaneKeepsItsScrollAcrossARedraw(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 25, 16, 0, 0, 0, time.Local)
	var recent []map[string]any
	for i := 0; i < 30; i++ {
		recent = append(recent, map[string]any{
			"at":      now.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339),
			"bead":    fmt.Sprintf("gt-%d", i),
			"rig":     "gastown",
			"outcome": "landed",
		})
	}
	state := map[string]any{"trend": map[string]any{
		"hours":  []map[string]any{{"hour": now.Truncate(time.Hour).Format(time.RFC3339), "landed": 30, "rejected": 0}},
		"stages": []any{},
		"recent": recent,
	}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}

	p := newPageDOM(t)
	p.call(t, "renderTrend", st)
	if _, err := p.vm.RunString(`document.getElementById("trend").querySelector(".lrows").scrollTop = 240`); err != nil {
		t.Fatalf("scrolling the landings rows down: %v", err)
	}
	p.call(t, "renderTrend", st)

	v, err := p.vm.RunString(`document.getElementById("trend").querySelector(".lrows").scrollTop`)
	if err != nil {
		t.Fatalf("reading the landings rows' scroll position: %v", err)
	}
	if got := v.ToInteger(); got != 240 {
		t.Errorf("landings scroll position after a redraw = %d, want 240: the poll put the rows back at the top", got)
	}
}

// landingsReading is a Landings pane reading with something landed in the
// window, so the figures, the per-hour bars and the table are all drawn: two
// hours of landings, each with a load reading, two stage lines, and one
// finished landing.
func landingsReading(now time.Time) map[string]any {
	return map[string]any{"trend": map[string]any{
		"hours": []map[string]any{
			{"hour": now.Add(-time.Hour).Format(time.RFC3339), "landed": 3, "rejected": 1, "load_avg": 2.0},
			{"hour": now.Add(-30 * time.Minute).Format(time.RFC3339), "landed": 2, "rejected": 0, "load_avg": 4.0},
		},
		"stages": []any{
			map[string]any{"at": now.Add(-50 * time.Minute).Format(time.RFC3339), "lint_secs": 10, "gate_secs": 60, "om_secs": 30},
			map[string]any{"at": now.Add(-20 * time.Minute).Format(time.RFC3339), "lint_secs": 20, "gate_secs": 120, "om_secs": 90},
		},
		"recent": []any{
			map[string]any{"at": now.Add(-10 * time.Minute).Format(time.RFC3339), "bead": "gt-landed", "outcome": "landed"},
		},
	}}
}

// queuedBeads is n beads waiting for the landing worker, each last updated a
// minute apart so the table has an order.
func queuedBeads(now time.Time, n int) []map[string]any {
	var out []map[string]any
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{
			"id":         fmt.Sprintf("gt-q%d", i),
			"updated_at": now.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339),
		})
	}
	return out
}

// The Landings header says how many beads are queued, next to the day's landed
// and rejected counts: the operator sees the depth of the queue without opening
// anything. The count is the waiting rows the table lists first, and it sits in
// the heading rather than in the foldable head, so closing the head leaves it
// in view (gt-q4cac).
func TestLandingsHeaderCarriesTheQueuedCount(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	st := landingsReading(now)
	st["queue"] = map[string]any{"landing": queuedBeads(now, 3)}

	p := newPageDOM(t)
	p.call(t, "renderTrend", st)

	note := p.element(t, "trendnote")
	if want := "5 landed · 1 rejected · 3 queued"; note.Text != want {
		t.Errorf("the Landings header reads %q, want %q", note.Text, want)
	}
	if note.Hidden {
		t.Error("the Landings header is hidden while the pane is open")
	}

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var rows = 0, outside = true, node = __byId["trendnote"];
  __walk(document.getElementById("trend"), function (c) {
    if (c.tagName === "SPAN" && __cls(c, "tag") && c.textContent === "queued") rows++;
  });
  while (node) { if (__cls(node, "lhead")) outside = false; node = node.parentNode || null; }
  return {rows: rows, outside: outside};
})())`)
	if err != nil {
		t.Fatalf("reading the landings header and rows: %v", err)
	}
	var got struct {
		Rows    int  `json:"rows"`
		Outside bool `json:"outside"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("decoding the landings header and rows: %v", err)
	}
	if got.Rows != 3 {
		t.Errorf("the table lists %d queued rows, want the header's 3", got.Rows)
	}
	if !got.Outside {
		t.Error("the queued count sits inside the collapsible head")
	}

	// Collapsing the head leaves the heading alone, so the count stays readable
	// with the figures put away.
	if _, err := p.vm.RunString(`__fireOn(__find(document.getElementById("trend"), ".lheadbtn"), "click", {type: "click"})`); err != nil {
		t.Fatalf("closing the landings head: %v", err)
	}
	if after := p.element(t, "trendnote"); after.Text != note.Text || after.Hidden {
		t.Errorf("with the head closed the header reads %q (hidden=%v), want %q", after.Text, after.Hidden, note.Text)
	}
}

// landingsHeadView is the collapsible head as a test reads it back: whether the
// head, its button and its body are drawn, whether the stats row and the bars
// are inside it, whether the table is, and the state its button carries.
type landingsHeadView struct {
	Head        bool   `json:"head"`
	Button      bool   `json:"button"`
	Body        bool   `json:"body"`
	KVInside    bool   `json:"kvInside"`
	BarsInside  bool   `json:"barsInside"`
	TableInside bool   `json:"tableInside"`
	BodyHidden  bool   `json:"bodyHidden"`
	TableDrawn  bool   `json:"tableDrawn"`
	TableHidden bool   `json:"tableHidden"`
	Aria        string `json:"aria"`
	Caret       string `json:"caret"`
}

// readLandingsHead reads the head's shape and state back from the drawn pane.
func readLandingsHead(t *testing.T, p *pageDOM) landingsHeadView {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var trend = document.getElementById("trend");
  var head = __find(trend, ".lhead");
  var btn = head && __find(head, ".lheadbtn");
  var body = head && __find(head, ".lheadbody");
  var kv = head && __find(head, ".kv");
  var bars = head && __find(head, ".hbars");
  var tbl = __find(trend, ".ltable");
  var caret = btn && __find(btn, ".caret");
  function within(node, root) { while (node) { if (node === root) return true; node = node.parentNode || null; } return false; }
  return {
    head: !!head, button: !!btn, body: !!body,
    kvInside: !!kv && within(kv, head),
    barsInside: !!bars && within(bars, head),
    tableInside: !!tbl && within(tbl, head),
    bodyHidden: body ? !!body.hidden : false,
    tableDrawn: !!tbl,
    tableHidden: tbl ? !!tbl.hidden : false,
    aria: btn ? String(btn.getAttribute("aria-expanded")) : "",
    caret: caret ? caret.textContent : ""
  };
})())`)
	if err != nil {
		t.Fatalf("reading the drawn landings head: %v", err)
	}
	var got landingsHeadView
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("decoding the drawn landings head: %v", err)
	}
	return got
}

// The day's figures and the per-hour bars sit in one head the reader can fold
// away; the landings table below never folds, so the queue and the finished
// rows stay in view while the figures are put away (gt-q4cac).
func TestLandingsHeadFoldsTheFiguresAndBarsButNotTheTable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsReading(now))

	before := readLandingsHead(t, p)
	if !before.Head || !before.Button || !before.Body {
		t.Fatalf("the pane draws head=%v button=%v body=%v, want the collapsible head and its button", before.Head, before.Button, before.Body)
	}
	if !before.KVInside || !before.BarsInside {
		t.Errorf("the stats row inside=%v and the bars inside=%v, want both in the collapsible head", before.KVInside, before.BarsInside)
	}
	if before.TableInside {
		t.Error("the landings table sits inside the collapsible head")
	}
	if !before.TableDrawn || before.TableHidden {
		t.Error("the landings table is not drawn and visible")
	}
	if before.BodyHidden {
		t.Error("the head starts closed for a reader with no saved choice")
	}
	if before.Aria != "true" || before.Caret != "▾" {
		t.Errorf("the open head's button reads aria-expanded=%q caret=%q, want true and ▾", before.Aria, before.Caret)
	}

	if _, err := p.vm.RunString(`__fireOn(__find(document.getElementById("trend"), ".lheadbtn"), "click", {type: "click"})`); err != nil {
		t.Fatalf("closing the landings head: %v", err)
	}
	after := readLandingsHead(t, p)
	if !after.BodyHidden {
		t.Error("the head's body is still shown after the reader closed it")
	}
	if after.Aria != "false" || after.Caret != "▸" {
		t.Errorf("the closed head's button reads aria-expanded=%q caret=%q, want false and ▸", after.Aria, after.Caret)
	}
	if !after.TableDrawn || after.TableHidden || after.TableInside {
		t.Error("closing the head took the landings table with it")
	}
}

// The head's choice is remembered per viewer, so a reader who puts the figures
// away finds them away after a reload; a browser that refuses storage reads as
// no choice, the head starts open, and folding still works for the session
// (gt-q4cac).
func TestLandingsHeadKeepsTheReadersChoice(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	first := newPageDOM(t)
	first.call(t, "renderTrend", landingsReading(now))
	if _, err := first.vm.RunString(`__fireOn(__find(document.getElementById("trend"), ".lheadbtn"), "click", {type: "click"})`); err != nil {
		t.Fatalf("closing the landings head: %v", err)
	}
	if got := first.storage(t)["gt-landings-head"]; got != "folded" {
		t.Fatalf("the closed head saved %q, want folded", got)
	}

	again := newPageDOMStoring(t, first.storage(t))
	again.call(t, "renderTrend", landingsReading(now))
	if !readLandingsHead(t, again).BodyHidden {
		t.Error("a saved choice to close the head did not survive the reload")
	}

	refusing := newPageDOMNoStorage(t)
	refusing.call(t, "renderTrend", landingsReading(now))
	view := readLandingsHead(t, refusing)
	if !view.Body || view.BodyHidden {
		t.Fatalf("a browser that refuses storage draws body=%v hidden=%v, want an open head", view.Body, view.BodyHidden)
	}
	if _, err := refusing.vm.RunString(`__fireOn(__find(document.getElementById("trend"), ".lheadbtn"), "click", {type: "click"})`); err != nil {
		t.Fatalf("closing the head without storage: %v", err)
	}
	if !readLandingsHead(t, refusing).BodyHidden {
		t.Error("folding does not work for the session when storage is refused")
	}
}

// The gate-and-om chart and the hourly load chart are gone, and the
// landings-per-hour bars stand exactly as they were: one column per hour with
// the landed part blue and the rejected part red (gt-q4cac).
func TestLandingsPaneDropsTheTwoLowerChartsAndKeepsTheBars(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsReading(now))

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var trend = document.getElementById("trend"), svgs = 0, hbars = 0, cols = 0, ok = 0, rej = 0;
  (function walk(e) {
    for (var i = 0; i < (e.children || []).length; i++) {
      var c = e.children[i];
      if (c.tagName === "SVG") svgs++;
      if (__cls(c, "hbars")) hbars++;
      if (__cls(c, "hbar")) cols++;
      if (__cls(c, "hok")) ok++;
      if (__cls(c, "hrej")) rej++;
      walk(c);
    }
  })(trend);
  return {svgs: svgs, hbars: hbars, cols: cols, ok: ok, rej: rej, text: trend.textContent};
})())`)
	if err != nil {
		t.Fatalf("reading the drawn landings charts: %v", err)
	}
	var got struct {
		SVGs  int    `json:"svgs"`
		HBars int    `json:"hbars"`
		Cols  int    `json:"cols"`
		OK    int    `json:"ok"`
		Rej   int    `json:"rej"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("decoding the drawn landings charts: %v", err)
	}
	if got.SVGs != 0 {
		t.Errorf("the pane draws %d chart(s), want the two lower charts gone", got.SVGs)
	}
	if got.HBars != 1 || got.Cols != 2 || got.OK != 2 || got.Rej != 1 {
		t.Errorf("the per-hour bar chart is hbars=%d columns=%d blue=%d red=%d, want 1, 2, 2, 1", got.HBars, got.Cols, got.OK, got.Rej)
	}
	for _, gone := range []string{"gate and om time per landing", "hourly load average"} {
		if strings.Contains(got.Text, gone) {
			t.Errorf("the pane still reads %q, want the chart's caption gone", gone)
		}
	}
	// The compact lines replace the charts through renderTrend's own call, so a
	// render that stopped drawing them fails here as well as at the helper.
	for _, want := range []string{
		"landings per hour, red = rejected",
		"gate · median 2m · p90 2m · slowest 2m  om · median 60s · p90 2m · slowest 2m",
		"load · now 4.0 · 24 hour average 3.0 · peak 4.0 · 2 of 24 hours recorded (only while a page is open)",
	} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("the pane does not read %q", want)
		}
	}
}

// With nothing landed in the window the pane is the queue alone, and its
// header still says how deep the queue is — the figure the operator opened the
// pane for (gt-q4cac).
func TestLandingsHeaderCountsTheQueueWhenOnlyTheQueueIsShown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", map[string]any{
		"trend": map[string]any{"hours": []any{}, "stages": []any{}, "recent": []any{}},
		"queue": map[string]any{"landing": queuedBeads(now, 2)},
	})

	note := p.element(t, "trendnote")
	if note.Text != "2 queued" {
		t.Errorf("the queue-only header reads %q, want 2 queued", note.Text)
	}
	if note.Hidden {
		t.Error("the queue-only header is hidden")
	}
}

// The two lower charts are replaced by one compact stats line each, computed
// from the same reading: the gate and om median, 90th percentile and slowest
// over the landings in the window, and the load now, 24 hour average and peak
// over the hours on record. The values are read back from fixed input, so a
// line that stopped reading the fields fails on the figure that changed
// (gt-q4cac).
func TestLandingsCompactStatsReadTheWindow(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	for _, c := range []struct {
		name string
		expr string
		want string
	}{
		{
			"gate median, p90 and slowest",
			`stageSummary("gate", [30, 60, 90, 120])`,
			"gate · median 75s · p90 2m · slowest 2m",
		},
		{
			"om median, p90 and slowest",
			`stageSummary("om", [10, 20, 30, 40])`,
			"om · median 25s · p90 40s · slowest 40s",
		},
		{
			// "now" is the latest reading, not the last one in the array.
			"load now, average and peak",
			`loadSummary([{t: 3, v: 5.0}, {t: 1, v: 2.0}, {t: 2, v: 3.0}])`,
			"load · now 5.0 · 24 hour average 3.3 · peak 5.0 · 3 of 24 hours recorded (only while a page is open)",
		},
		{
			"a stage an om review skipped is left out",
			`stageSummary("om", [null, 40, null])`,
			"om · median 40s · p90 40s · slowest 40s",
		},
	} {
		if got := pageExpr(t, p, c.expr); got != c.want {
			t.Errorf("%s: %s = %q, want %q", c.name, c.expr, got, c.want)
		}
	}
}

// With no stage line or no load reading the compact lines say so rather than
// reading as a zero: an empty window is not a fast landing and not an idle
// machine (gt-q4cac).
func TestLandingsCompactStatsRefuseAnEmptyWindow(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	if got := pageExpr(t, p, `String(stageSummary("gate", []))`); got != "null" {
		t.Errorf("stageSummary with no readings = %s, want null", got)
	}
	if got := pageExpr(t, p, `String(stageSummary("om", [null, null]))`); got != "null" {
		t.Errorf("stageSummary with only skipped reviews = %s, want null", got)
	}
	if got := pageExpr(t, p, `String(loadSummary([]))`); got != "null" {
		t.Errorf("loadSummary with no readings = %s, want null", got)
	}

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p.call(t, "renderTrend", map[string]any{"trend": map[string]any{
		"hours":  []map[string]any{{"hour": now.Format(time.RFC3339), "landed": 1}},
		"stages": []any{},
	}})
	text := p.element(t, "trend")
	if !strings.Contains(text.Text, "no stage lines in the last 24 hours") {
		t.Error("a window with no stage lines does not say so")
	}
	if !strings.Contains(text.Text, "no load history yet (it fills while a page is open)") {
		t.Error("a window with no load readings does not say so")
	}
}

// With nothing landed in the window the pane is the queue alone, and renderTrend
// takes its own early return to draw it. That redraw must keep the reader's place
// too (gt-tjlu6). Ten queued beads plus the header are 264px of rows in a 200px
// viewport, so a scroll of 40 is inside the range the page can reach.
func TestLandingsPaneKeepsItsScrollWhenOnlyTheQueueIsShown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 25, 16, 0, 0, 0, time.Local)
	var queued []map[string]any
	for i := 0; i < 10; i++ {
		queued = append(queued, map[string]any{
			"id":         fmt.Sprintf("gt-q%d", i),
			"updated_at": now.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339),
		})
	}
	state := map[string]any{
		"trend": map[string]any{"hours": []any{}, "stages": []any{}, "recent": []any{}},
		"queue": map[string]any{"landing": queued},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}

	p := newPageDOM(t)
	p.call(t, "renderTrend", st)
	if _, err := p.vm.RunString(`document.getElementById("trend").querySelector(".lrows").scrollTop = 40`); err != nil {
		t.Fatalf("scrolling the queued rows down: %v", err)
	}
	p.call(t, "renderTrend", st)

	v, err := p.vm.RunString(`document.getElementById("trend").querySelector(".lrows").scrollTop`)
	if err != nil {
		t.Fatalf("reading the queued rows' scroll position: %v", err)
	}
	if got := v.ToInteger(); got != 40 {
		t.Errorf("queue-only scroll position after a redraw = %d, want 40: the early return put the rows back at the top", got)
	}
}
