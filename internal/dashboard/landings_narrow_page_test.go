package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The Landings table has eight columns and the page is drawn in a dashboard
// column as narrow as 470 px. At that width the table must stay readable: the
// .lrows box scrolls it sideways (the Cost table's own fade marks the cut
// edge), a When cell reads as one value, and a title keeps a floor instead of
// breaking into fragments (gt-u7xe9).
//
// These tests read the stylesheet. The page harness has no layout engine, so a
// test cannot see what a browser draws: it asserts the declarations that
// produce the layout, at both the wide two-track .grid and the narrow stacked
// one. Each test fails when its declaration is removed or changed to a value
// that draws a different table. The rendered check — that the table really
// scrolls and the title really keeps its floor in a 470 px column — is a live
// check the overseer owes after install (gt-o7z1j).

// landingsBodyRow is one body row of the Landings table as the page drew it:
// its class and each cell's class, text, hover and column span.
type landingsBodyRow struct {
	Class string `json:"class"`
	Cells []struct {
		Class string `json:"class"`
		Text  string `json:"text"`
		Title string `json:"title"`
		Span  int    `json:"span"`
	} `json:"cells"`
}

// landingsRows reads back the body rows the Landings table drew, in order.
func landingsRows(t *testing.T, p *pageDOM) []landingsBodyRow {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var rows = [];
  __walk(document.getElementById("trend"), function (c) {
    if (c.tagName !== "TR" || !c.children.length || c.children[0].tagName === "TH") return;
    rows.push({class: c.className || "", cells: c.children.map(function (td) {
      return {class: td.className || "", text: td.textContent, title: td.title || "", span: td.colSpan || 1};
    })});
  });
  return rows;
})())`)
	if err != nil {
		t.Fatalf("reading the landings rows: %v", err)
	}
	var rows []landingsBodyRow
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the landings rows: %v", err)
	}
	return rows
}

// landingsQueuedRunningAndLanded is a reading whose table holds all three kinds
// of row the narrow column draws: a bead waiting to land, the landing the worker
// is running now, and one it finished.
func landingsQueuedRunningAndLanded(now time.Time) map[string]any {
	return map[string]any{
		"queue": map[string]any{"landing": queuedBeads(now, 1)},
		"trend": map[string]any{
			"hours": []map[string]any{
				{"hour": now.Add(-time.Hour).Format(time.RFC3339), "landed": 2, "rejected": 0},
			},
			"stages": []any{},
			"recent": []any{
				map[string]any{"at": now.Add(-30 * time.Second).Format(time.RFC3339), "bead": "gt-running", "outcome": "running"},
				map[string]any{"at": now.Add(-10 * time.Minute).Format(time.RFC3339), "bead": "gt-landed", "outcome": "landed"},
			},
		},
	}
}

// The landings table is drawn inside the .lrows box, whose rule scrolls it
// sideways with the same dim fade the Cost table uses, so at 470 px the om and
// Ship columns are reachable instead of cut off.
func TestLandingsTableScrollsSidewaysAtANarrowWidth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsReading(now))

	decls := cssDecls(t, "lrows")
	if got := decls["overflow-x"]; got != "auto" {
		t.Errorf("the .lrows rule has overflow-x %q, want auto so a narrow column scrolls the table instead of clipping its last columns", got)
	}
	fade := decls["background"]
	if !strings.Contains(fade, "radial-gradient") || !strings.Contains(fade, "var(--dim)") {
		t.Errorf("the .lrows rule has background %q, want the dim radial gradient at each edge that cues the sideways scroll", fade)
	}
	if !strings.Contains(fade, "var(--panel)") {
		t.Errorf("the .lrows rule has background %q, want the panel-coloured cover over the fade so it is hidden when nothing is cut", fade)
	}
	if got, want := cssLayers(decls["background-attachment"]), []string{"local", "local", "scroll", "scroll"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the .lrows rule has background-attachment %q, want %v", decls["background-attachment"], want)
	}

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var tbl = __find(document.getElementById("trend"), ".ltable");
  var wrap = tbl && tbl.parentNode;
  return {drawn: !!tbl, wrapped: !!wrap && __cls(wrap, "lrows")};
})())`)
	if err != nil {
		t.Fatalf("reading where the landings table is drawn: %v", err)
	}
	var got struct {
		Drawn   bool `json:"drawn"`
		Wrapped bool `json:"wrapped"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading where the landings table is drawn: %v", err)
	}
	if !got.Drawn {
		t.Fatal("the landings pane drew no table")
	}
	if !got.Wrapped {
		t.Error("the landings table is not drawn in the .lrows scroll box, so a narrow column cannot scroll it sideways")
	}
}

// A When cell is one reading: a running row's "merging 47s" and a finished
// row's time are both nowrap, so neither wraps over two lines at 470 px.
func TestLandingsWhenCellNeverWraps(t *testing.T) {
	t.Parallel()

	if got := cssDecls(t, "when")["white-space"]; got != "nowrap" {
		t.Errorf("the When cell rule has white-space %q, want nowrap so \"merging 47s\" stays on one line", got)
	}

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsQueuedRunningAndLanded(now))

	rows := landingsRows(t, p)
	if len(rows) != 3 {
		t.Fatalf("the table drew %d rows, want the queued bead, the running landing and the finished one", len(rows))
	}
	for _, row := range rows {
		if len(row.Cells) == 0 {
			t.Fatal("a landings row drew no cells")
		}
		if !hasClassToken(row.Cells[0].Class, "when") {
			t.Errorf("the When cell %q of row %q carries class %q, want the when class the nowrap rule names", row.Cells[0].Text, row.Cells[1].Text, row.Cells[0].Class)
		}
	}
	if got := rows[1].Cells[0].Text; !strings.HasPrefix(got, "merging ") {
		t.Errorf("the running row's When cell reads %q, want the merging reading", got)
	}
	if got := rows[2].Cells[0].Text; strings.TrimSpace(got) == "" {
		t.Errorf("the finished row's When cell reads %q, want the landing's time", got)
	}
}

// A landing's title keeps a floor: the Landing cell is at least 200 px wide and
// only breaks a word when nothing else fits, so a narrow column does not shred
// it into one- and two-letter lines. A long unbroken token is the case the
// break-word value is chosen for: it breaks rather than overflowing the cell.
func TestLandingsTitleCellKeepsAReadableWidth(t *testing.T) {
	t.Parallel()

	decls := cssDecls(t, "main")
	min, ok := decls["min-width"]
	if !ok {
		t.Fatal("the Landing cell rule sets no min-width, so a narrow column squeezes the title to nothing")
	}
	if px := cssPxValue(t, min); px < 150 {
		t.Errorf("the Landing cell rule has min-width %q, want at least 150px for a readable title", min)
	}
	if got := decls["overflow-wrap"]; got != "break-word" {
		t.Errorf("the Landing cell rule has overflow-wrap %q, want break-word: a long word breaks only as a last resort, never anywhere", got)
	}

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsQueuedRunningAndLanded(now))

	rows := landingsRows(t, p)
	if len(rows) != 3 {
		t.Fatalf("the table drew %d rows, want the queued bead, the running landing and the finished one", len(rows))
	}
	for _, row := range rows {
		if len(row.Cells) < 2 {
			t.Fatal("a landings row drew fewer than two cells")
		}
		if !hasClassToken(row.Cells[1].Class, "main") {
			t.Errorf("the Landing cell of row %q carries class %q, want the main class the title rule names", row.Cells[0].Text, row.Cells[1].Class)
		}
	}
}

// The table sizes its columns from their content. That is what turns the
// title's floor and the When cell's nowrap into width: a fixed layout would
// size the columns from the table width and ignore both, squeezing the title
// and clipping the When reading however the cells declare themselves.
func TestLandingsTableSizesColumnsFromContent(t *testing.T) {
	t.Parallel()

	if got := cssDecls(t, "ltable")["table-layout"]; got != "auto" {
		t.Errorf("the .ltable rule has table-layout %q, want auto so the title floor and the When cell's nowrap size the columns", got)
	}
}

// landingsConditionalRule is one rule the page draws only under a width
// condition: the @media or @container block it sits in, its selector, and its
// declarations keyed by property.
type landingsConditionalRule struct {
	At    string
	Sel   string
	Decls map[string]string
}

// landingsConditionalRules reads every @media and @container block in the
// page's stylesheet. The page writes a rule's declarations with no nested
// blocks, so each block's rules split on their closing brace. A block that is
// opened and never closed is a fatal: the narrow layout cannot be read from a
// stylesheet that does not parse.
func landingsConditionalRules(t *testing.T) []landingsConditionalRule {
	t.Helper()

	css := pageStyle(t)
	var out []landingsConditionalRule
	for _, at := range []string{"@media", "@container"} {
		for rest := css; ; {
			start := strings.Index(rest, at)
			if start < 0 {
				break
			}
			open := strings.IndexByte(rest[start:], '{')
			if open < 0 {
				t.Fatalf("index.html has an unclosed %s block", at)
			}
			open += start
			atRule := strings.TrimSpace(rest[start:open])
			body, n := landingsBracedBody(t, rest[open:])
			for _, rule := range strings.Split(body, "}") {
				sel, declText, ok := strings.Cut(rule, "{")
				if !ok {
					continue
				}
				decls := map[string]string{}
				for _, decl := range strings.Split(declText, ";") {
					if prop, val, ok := strings.Cut(decl, ":"); ok {
						decls[strings.TrimSpace(prop)] = strings.TrimSpace(val)
					}
				}
				out = append(out, landingsConditionalRule{At: atRule, Sel: strings.TrimSpace(sel), Decls: decls})
			}
			rest = rest[open+n:]
		}
	}
	return out
}

// landingsBracedBody returns the text between s[0], a "{", and its matching
// close brace, and the index just past that brace.
func landingsBracedBody(t *testing.T, s string) (string, int) {
	t.Helper()

	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[1:i], i + 1
			}
		}
	}
	t.Fatalf("index.html has an unclosed block")
	return "", 0
}

// The table is drawn in the wide two-track .grid and in the narrow column the
// same .grid stacks to one track below 1000 px. No width-conditional rule
// re-declares a landings layout property, so the two columns draw the same
// table rules and a check of the declarations covers both widths. What differs
// between them is only the width they leave the table, which these tests cannot
// see; the overseer's live 470 px check covers the narrow one.
func TestLandingsLayoutRulesHoldAtEveryWidth(t *testing.T) {
	t.Parallel()

	wide := declOf(t, topRule(t, ".grid"), "grid-template-columns")
	if got := len(strings.Fields(wide)); got != 2 {
		t.Errorf("the .grid draws %d tracks (%q), want the two the landings panel shares at a wide width", got, wide)
	}

	stacked := false
	for _, rule := range landingsConditionalRules(t) {
		if rule.Sel == ".grid" && strings.Contains(rule.At, "max-width") && rule.Decls["grid-template-columns"] == "1fr" {
			stacked = true
		}
	}
	if !stacked {
		t.Error("no max-width rule stacks the .grid to one track, so the narrow column the landings table is drawn in has no declaration")
	}

	props := []string{"table-layout", "min-width", "overflow-wrap", "white-space", "overflow-x"}
	for _, rule := range landingsConditionalRules(t) {
		for _, class := range []string{"ltable", "lrows", "main", "when"} {
			if !selectsClass(rule.Sel, class) {
				continue
			}
			for _, prop := range props {
				if got, ok := rule.Decls[prop]; ok {
					t.Errorf("%s sets %s:%s on %s, so the table draws a different layout at that width than the declarations the other tests pin", rule.At, prop, got, rule.Sel)
				}
			}
		}
	}
}

// landingsHeaderCells is how many columns the Landings table heads: the count a
// row's cells and spans must add up to for its cells to sit under the headers.
func landingsHeaderCells(t *testing.T, p *pageDOM) int {
	t.Helper()

	v, err := p.vm.RunString(`(function () {
  var tbl = __find(document.getElementById("trend"), ".ltable");
  return tbl ? tbl.children[0].children.length : 0;
})()`)
	if err != nil {
		t.Fatalf("reading the landings header: %v", err)
	}
	return int(v.ToInteger())
}

// A waiting row has no stage readings, so its five cost cells held nothing but
// dashes and its title sat in the Landing column alone, wrapping over six or
// seven lines. The title cell now spans the columns those cells and By would
// have taken, so the title reads in a line or two (gt-uvpo1).
func TestAWaitingRowTitleCellSpansTheEmptyCells(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsQueuedRunningAndLanded(now))

	head := landingsHeaderCells(t, p)
	if head != 8 {
		t.Fatalf("the table heads %d columns, want the eight the Landings table draws", head)
	}
	rows := landingsRows(t, p)
	if len(rows) != 3 {
		t.Fatalf("the table drew %d rows, want the queued bead, the running landing and the finished one", len(rows))
	}
	waiting := rows[0]
	if len(waiting.Cells) != 2 {
		t.Fatalf("the waiting row drew %d cells, want its When cell and one title cell", len(waiting.Cells))
	}
	if got := waiting.Cells[0].Span + waiting.Cells[1].Span; got != head {
		t.Errorf("the waiting row covers %d columns, want the %d the table heads", got, head)
	}
	if got := waiting.Cells[1].Span; got != head-1 {
		t.Errorf("the waiting row's title cell spans %d columns, want %d: it takes the five cost cells and By", got, head-1)
	}
	if !hasClassToken(waiting.Cells[1].Class, "main") {
		t.Errorf("the waiting row's title cell carries class %q, want the main class the title rule names", waiting.Cells[1].Class)
	}
}

// waitingRowParts is what one waiting row's cells read back: the parts of its
// title cell, and the When cell beside it.
type waitingRowParts struct {
	When  drawn  `json:"when"`
	Bead  drawn  `json:"bead"`
	Tag   drawn  `json:"tag"`
	By    *drawn `json:"by"`
	Title drawn  `json:"title"`
}

// waitingRow reads back the first waiting row's When cell and the parts its
// title cell drew.
func waitingRow(t *testing.T, p *pageDOM) waitingRowParts {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var tbl = __find(document.getElementById("trend"), ".ltable");
  var tr = tbl.children[1], main = tr.children[1];
  function part(e) { return e ? {text: e.textContent, class: e.className || "", title: e.title || ""} : null; }
  return {when: part(tr.children[0]), bead: part(__find(main, ".bead")), tag: part(__find(main, ".tag")),
          by: part(__find(main, ".by")), title: part(__find(main, ".title"))};
})())`)
	if err != nil {
		t.Fatalf("reading the waiting row: %v", err)
	}
	var got waitingRowParts
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the waiting row: %v", err)
	}
	return got
}

// The span does not cost the row a field: the When cell still ages the wait from
// the bead's last update, and the title cell still carries the bead id, the
// queued tag and why it says that, the holder with the store it names, and the
// rig and title of the bead (gt-uvpo1).
func TestAWaitingRowKeepsEveryFieldItsSpanCovers(t *testing.T) {
	t.Parallel()

	// The page ages the wait against its own clock, which starts at the real one
	// when the page loads, so the bead's last update is set from the real clock.
	p := newPageDOM(t)
	p.call(t, "renderTrend", map[string]any{
		"queue": map[string]any{"landing": []any{map[string]any{
			"id": "gt-loqew", "title": "the queue that never drains",
			"rig": "gastown", "assignee": "gastown/polecats/dag",
			"updated_at": time.Now().Add(-12 * time.Minute).Format(time.RFC3339),
		}}},
	})

	got := waitingRow(t, p)
	if got.When.Text != "waiting 12m" {
		t.Errorf("the When cell reads %q, want the wait aged from the bead's last update", got.When.Text)
	}
	if !strings.Contains(got.When.Title, "waiting since the bead's last update") {
		t.Errorf("the When cell's hover reads %q, want it to say what the wait is aged from", got.When.Title)
	}
	if got.Bead.Text != "gt-loqew" {
		t.Errorf("the title cell's bead reads %q, want the queued bead's id", got.Bead.Text)
	}
	if got.Tag.Text != "queued" || !strings.Contains(got.Tag.Title, "the landing worker has not taken it") {
		t.Errorf("the title cell's tag reads %q (%q), want the queued tag and why it says that", got.Tag.Text, got.Tag.Title)
	}
	if got.By == nil {
		t.Fatal("the title cell draws no holder: the row lost the By reading it carried in the By column")
	}
	if got.By.Text != "dag" || got.By.Title != "gastown/dag" {
		t.Errorf("the title cell's holder reads %q (%q), want the bare name and the store it names", got.By.Text, got.By.Title)
	}
	if got.Title.Text != "gastown · the queue that never drains" {
		t.Errorf("the title cell's title reads %q, want the bead's rig and title", got.Title.Text)
	}
}

// A bead nobody holds draws no holder rather than a placeholder for one: the
// row says waiting, and its title cell carries no "by" reading (gt-uvpo1).
func TestAWaitingRowWithNoHolderDrawsNoByReading(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", map[string]any{
		"queue": map[string]any{"landing": []any{map[string]any{
			"id": "gt-nowho", "updated_at": now.Add(-5 * time.Minute).Format(time.RFC3339),
		}}},
	})

	rows := landingsRows(t, p)
	if len(rows) != 1 {
		t.Fatalf("the table drew %d rows, want the one queued bead", len(rows))
	}
	if len(rows[0].Cells) != 2 {
		t.Fatalf("the waiting row drew %d cells, want its When cell and one title cell", len(rows[0].Cells))
	}
	if got := rows[0].Cells[1].Text; strings.Contains(got, "· by") {
		t.Errorf("the title cell reads %q, want no holder reading for a bead nobody holds", got)
	}
	if got := waitingRow(t, p); got.By != nil {
		t.Errorf("the title cell draws a holder %q, want none for a bead nobody holds", got.By.Text)
	}
}

// The span is the waiting row's alone: a finished landing and one the worker is
// running still draw all eight cells, with the readings the server sent and
// dashes where a stage has not run (gt-uvpo1).
func TestFinishedAndRunningRowsKeepEveryCell(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", map[string]any{
		"queue": map[string]any{"landing": queuedBeads(now, 1)},
		"trend": map[string]any{
			"hours":  []map[string]any{{"hour": now.Add(-time.Hour).Format(time.RFC3339), "landed": 1, "rejected": 0}},
			"stages": []any{},
			"recent": []any{
				map[string]any{"at": now.Add(-30 * time.Second).Format(time.RFC3339), "bead": "gt-running", "outcome": "running", "polecat": "dag"},
				map[string]any{"at": now.Add(-10 * time.Minute).Format(time.RFC3339), "bead": "gt-landed", "outcome": "landed", "polecat": "dag",
					"score": 0.86, "gate_secs": 42.0, "om_secs": 61.0, "ship_secs": 180.0},
			},
		},
	})

	head := landingsHeaderCells(t, p)
	rows := landingsRows(t, p)
	if len(rows) != 3 {
		t.Fatalf("the table drew %d rows, want the queued bead, the running landing and the finished one", len(rows))
	}
	for _, row := range rows[1:] {
		bead := row.Cells[1].Text
		if len(row.Cells) != head {
			t.Fatalf("the %q row drew %d cells, want the %d columns the table heads", bead, len(row.Cells), head)
		}
		for i, c := range row.Cells {
			if c.Span != 1 {
				t.Errorf("the %q row's cell %d spans %d columns, want 1: only a waiting row spans cells", bead, i, c.Span)
			}
		}
	}
	running, finished := rows[1], rows[2]
	if got := running.Cells[6].Text; got != "–" {
		t.Errorf("the running row's om cell reads %q, want a dash for a review that has not finished", got)
	}
	for i, want := range map[int]string{4: "0.86", 5: "42s", 6: "61s", 7: "3m"} {
		if got := finished.Cells[i].Text; got != want {
			t.Errorf("the finished row's cell %d reads %q, want %q", i, got, want)
		}
	}
}

// hasClassToken reports whether a space-separated class attribute names class.
func hasClassToken(attr, class string) bool {
	for _, c := range strings.Fields(attr) {
		if c == class {
			return true
		}
	}
	return false
}
