package dashboard

import (
	"encoding/json"
	"regexp"
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
    rows.push({class: c.className || "", cells: c.children.__arr.map(function (td) {
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

// A queued row draws the same cells in the same columns as a running one: When,
// Landing, By, Polecat, Score, Gate, om and Ship, every cell one column wide.
// The holder sits in By, so the Landing cell carries no "by <name>" text, and
// the columns with no queued reading hold the dash a running row uses for a
// column it has no value for (gt-duuq1). Mutation: drop the queued row's By cell
// or one of its dashes and the row no longer covers the header's columns.
func TestAQueuedRowDrawsTheRunningRowsCells(t *testing.T) {
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
	queued, running := rows[0], rows[1]
	if len(queued.Cells) != head {
		t.Fatalf("the queued row drew %d cells, want the %d the table heads", len(queued.Cells), head)
	}
	for i, c := range queued.Cells {
		if c.Span != 1 {
			t.Errorf("the queued row's cell %d spans %d columns, want 1 as the running row's holds", i, c.Span)
		}
		if c.Class != running.Cells[i].Class {
			t.Errorf("the queued row's cell %d carries class %q, want the running row's %q in that column", i, c.Class, running.Cells[i].Class)
		}
	}
	if got := queued.Cells[1].Text; strings.Contains(got, "by ") {
		t.Errorf("the queued row's Landing cell reads %q, want no by reading: the holder sits in the By column", got)
	}
	for i := 2; i < head; i++ {
		if got := queued.Cells[i].Text; got != "–" {
			t.Errorf("the queued row's cell %d reads %q, want a dash for a stat the queue reading has not got", i, got)
		}
	}
}

// queuedRowParts is what one queued row reads back: its When and By cells, and
// the parts of the Landing cell the row draws inside.
type queuedRowParts struct {
	When    drawn `json:"when"`
	Landing drawn `json:"landing"`
	By      drawn `json:"by"`
	Bead    drawn `json:"bead"`
	Tag     drawn `json:"tag"`
	Title   drawn `json:"title"`
}

// queuedRow reads back the first queued row's cells and the parts of its
// Landing cell.
func queuedRow(t *testing.T, p *pageDOM) queuedRowParts {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var tbl = __find(document.getElementById("trend"), ".ltable");
  var tr = tbl.children[1], main = tr.children[1];
  function part(e) { return e ? {text: e.textContent, class: e.className || "", title: e.title || ""} : null; }
  return {when: part(tr.children[0]), landing: part(main), by: part(tr.children[2]),
          bead: part(__find(main, ".bead")), tag: part(__find(main, ".tag")), title: part(__find(main, ".title"))};
})())`)
	if err != nil {
		t.Fatalf("reading the queued row: %v", err)
	}
	var got queuedRowParts
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the queued row: %v", err)
	}
	return got
}

// The layout costs the row no field: the When cell ages the wait from the bead's
// last update, the Landing cell keeps the bead id, the queued tag and why it
// says that, and the rig and title of the bead, and the By cell names the holder
// with the store it names (gt-duuq1).
func TestAQueuedRowKeepsEveryField(t *testing.T) {
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

	got := queuedRow(t, p)
	if got.When.Text != "waiting 12m" {
		t.Errorf("the When cell reads %q, want the wait aged from the bead's last update", got.When.Text)
	}
	if !strings.Contains(got.When.Title, "submitted and waiting for the landing worker to take it") || !strings.Contains(got.When.Title, "waiting since the bead's last update") {
		t.Errorf("the When cell's hover reads %q, want it to say what the wait is aged from", got.When.Title)
	}
	if got.Bead.Text != "gt-loqew" {
		t.Errorf("the Landing cell's bead reads %q, want the queued bead's id", got.Bead.Text)
	}
	if got.Tag.Text != "queued" || !strings.Contains(got.Tag.Title, "the landing worker has not taken it") {
		t.Errorf("the Landing cell's tag reads %q (%q), want the queued tag and why it says that", got.Tag.Text, got.Tag.Title)
	}
	if got.Title.Text != "gastown · the queue that never drains" {
		t.Errorf("the Landing cell's title reads %q, want the bead's rig and title", got.Title.Text)
	}
	if strings.Contains(got.Landing.Text, "by ") {
		t.Errorf("the Landing cell reads %q, want no holder reading", got.Landing.Text)
	}
	if got.By.Text != "dag" || got.By.Title != "gastown/dag" {
		t.Errorf("the By cell reads %q (%q), want the bare name and the store it names", got.By.Text, got.By.Title)
	}
}

// A bead nobody holds draws the dash every other row draws for a holder, never
// a placeholder name, and the Landing cell carries no "by" reading (gt-duuq1).
func TestAQueuedRowWithNoHolderDrawsTheDash(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", map[string]any{
		"queue": map[string]any{"landing": []any{map[string]any{
			"id": "gt-nowho", "updated_at": now.Add(-5 * time.Minute).Format(time.RFC3339),
		}}},
	})

	got := queuedRow(t, p)
	if got.By.Text != "–" {
		t.Errorf("the By cell reads %q, want the dash a row with no holder draws", got.By.Text)
	}
	if got.By.Title != "" {
		t.Errorf("the By cell carries the hover %q, want none for a bead nobody holds", got.By.Title)
	}
	if strings.Contains(got.Landing.Text, "by ") {
		t.Errorf("the Landing cell reads %q, want no holder reading for a bead nobody holds", got.Landing.Text)
	}
}

// The queued row's Polecat cell is the shared dev-time computation applied to
// the bead: a reading the queue carries renders through polecatDur, its floor
// keeping the + (gt-duuq1). Mutation: hard-code the cell's dash and this row
// reads "–" where the reading says "13m+".
func TestAQueuedRowPolecatCellUsesTheSharedDevTime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", map[string]any{
		"queue": map[string]any{"landing": []any{map[string]any{
			"id": "gt-timed", "updated_at": now.Add(-2 * time.Minute).Format(time.RFC3339),
			"polecat_secs": 780.0, "polecat_lower_bound": true,
			"polecat_attempts": []any{map[string]any{"seat": "dag", "secs": 780.0, "lower_bound": true}},
		}}},
	})

	cells := landingsRows(t, p)[0].Cells
	if got := cells[3].Text; got != "13m+" {
		t.Errorf("the queued row's Polecat cell reads %q, want 13m+ as polecatDur reads a floor of 780s", got)
	}
	if title := cells[3].Title; !strings.Contains(title, "1 attempt: 13m+") {
		t.Errorf("the Polecat cell's hover reads %q, want the attempt polecatDur draws for a landed row", title)
	}
}

// A queue reading with no dev time for the bead reads the dash: the wait, the
// last update and the creation are not a start the polecat worked from, so no
// figure is invented for the cell (gt-duuq1).
func TestAQueuedRowWithNoDevTimeReadsTheDash(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", map[string]any{
		"queue": map[string]any{"landing": []any{map[string]any{
			"id": "gt-untimed", "updated_at": now.Add(-2 * time.Minute).Format(time.RFC3339),
		}}},
	})

	if got := landingsRows(t, p)[0].Cells[3].Text; got != "–" {
		t.Errorf("the queued row's Polecat cell reads %q, want a dash when the queue reading carries no dev time", got)
	}
}

// A finished landing and one the worker is running draw all eight cells, with
// the readings the server sent and dashes where a stage has not run.
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
	for _, row := range rows {
		bead := row.Cells[1].Text
		if len(row.Cells) != head {
			t.Fatalf("the %q row drew %d cells, want the %d columns the table heads", bead, len(row.Cells), head)
		}
		for i, c := range row.Cells {
			if c.Span != 1 {
				t.Errorf("the %q row's cell %d spans %d columns, want 1", bead, i, c.Span)
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

// The Landings pane is the one the operator reads daily, so it sits below the
// panel grid at the full page width, last on the page, and is the only panel
// that starts open (gt-bqxpz).
func TestLandingsIsFullWidthAndOnlyOpenPanel(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	at := strings.Index(page, `<section id="trendsec" class="wide" data-fold="landings" hidden>`)
	if at < 0 {
		t.Fatal("the Landings section is not the .wide section the page places below the grid")
	}
	start := strings.Index(page, `<div class="grid">`)
	if start < 0 || start > at {
		t.Fatal("the Landings section does not follow the panel grid")
	}
	between := page[start:at]
	if opens, closes := strings.Count(between, "<div"), strings.Count(between, "</div>"); opens != closes {
		t.Errorf("%d divs open and %d close between the grid and the Landings section: it sits inside the grid", opens, closes)
	}
	tail := page[at+len("<section"):]
	if end := strings.Index(tail, "<script>"); end < 0 || strings.Contains(tail[:end], "<section") {
		t.Error("a section follows the Landings section: it is not the last block")
	}
	for _, prop := range []string{"width", "max-width", "grid-column", "float"} {
		if got, ok := cssDecls(t, "wide")[prop]; ok {
			t.Errorf("the .wide rule sets %s:%s, want the section to take the page's full width as a block", prop, got)
		}
	}

	m := regexp.MustCompile(`const FOLD_DEFAULT_OPEN = \[([^\]]*)\];`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("index.html has no FOLD_DEFAULT_OPEN list")
	}
	if got := strings.Join(strings.Fields(m[1]), " "); got != `"polecats", "beads", "landings"` {
		t.Errorf("FOLD_DEFAULT_OPEN is [%s], want only the three panels below the grid: polecats, beads, landings", got)
	}

	dom := newPageDOM(t)
	for _, sp := range pageSections(t) {
		below := sp.Key == "polecats" || sp.Key == "beads" || sp.Key == "landings"
		if closed := dom.fold(t, sp.Key).Collapsed; closed == below {
			t.Errorf("the %s section starts collapsed=%v for a reader with no saved choice, want only polecats, beads and landings open", sp.Key, closed)
		}
	}
}

// At 470 px the eight columns cannot sit side by side. Below 640 px each row
// stacks: the title on a full line of its own, clamped to two lines, and every
// other cell named by its column because the header row is hidden. Nothing is
// cut and the page does not scroll sideways. The harness has no layout engine,
// so the test reads the declarations and the cells' labels; the rendered
// 470 px check is the overseer's, live (gt-bqxpz).
func TestLandingsFitsANarrowViewport(t *testing.T) {
	t.Parallel()

	want := map[string]map[string]string{
		".ltable":                {"display": "block"},
		".ltable tr":             {"display": "flex", "flex-wrap": "wrap"},
		".ltable tr:first-child": {"display": "none"},
		".ltable td":             {"display": "block"},
		".ltable td.main":        {"order": "-1", "flex": "1 0 100%"},
		".ltable td.main .title": {"-webkit-line-clamp": "2", "overflow": "hidden"},
	}
	found := map[string]bool{}
	for _, rule := range landingsConditionalRules(t) {
		if !strings.Contains(rule.At, "max-width:640px") {
			continue
		}
		// A selector list is read by its first selector: ".ltable td,.ltable td.n".
		first, _, _ := strings.Cut(rule.Sel, ",")
		for sel, decls := range want {
			if strings.TrimSpace(first) != sel {
				continue
			}
			found[sel] = true
			for prop, val := range decls {
				if got := rule.Decls[prop]; got != val {
					t.Errorf("%s sets %s:%q at a narrow width, want %q", sel, prop, got, val)
				}
			}
		}
	}
	for sel := range want {
		if !found[sel] {
			t.Errorf("no max-width:640px rule styles %s, so the narrow table keeps its eight columns side by side", sel)
		}
	}

	// A finished row's cells, except the title, are named for the stacked layout.
	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsQueuedRunningAndLanded(now))
	v, err := p.vm.RunString(`JSON.stringify((function () {
  var out = [];
  __walk(document.getElementById("trend"), function (c) {
    if (c.tagName !== "TR" || !c.children.length || c.children[0].tagName === "TH") return;
    out.push({title: c.title || "", labels: c.children.__arr.map(function (td) { return td["attr_data-label"] || ""; })});
  });
  return out;
})())`)
	if err != nil {
		t.Fatalf("reading the row labels: %v", err)
	}
	var rows []struct {
		Title  string   `json:"title"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the row labels: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("the table drew %d rows, want 3", len(rows))
	}
	wantLabels := []string{"When", "", "By", "Polecat", "Score", "Gate", "om", "Ship"}
	for _, row := range rows {
		if strings.Join(row.Labels, "|") != strings.Join(wantLabels, "|") {
			t.Errorf("a landings row's cells are labelled %q, want %q", row.Labels, wantLabels)
		}
		if row.Title != "" {
			t.Errorf("a landings row carries the hover %q on the whole row, want it on the bead id so the tip does not open over the row", row.Title)
		}
	}
}

// The stub's element.children answers what a browser's HTMLCollection does: a
// length, an index and iteration, and no array methods. The Landings pane once
// called children.forEach, which throws in a browser and stopped the table
// drawing while every page test passed on an array-backed stub (gt-bqxpz).
func TestStubChildrenIsACollectionWithNoArrayMethods(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	v, err := p.vm.RunString(`JSON.stringify((function () {
  var d = document.createElement("div"), a = document.createElement("span"), b = document.createElement("i");
  d.append(a, b);
  var out = {len: d.children.length, first: d.children[0] === a, item: d.children.item(1) === b, iter: 0};
  for (var c of d.children) out.iter++;
  out.methods = ["forEach", "map", "filter", "find", "slice"].filter(function (m) { return typeof d.children[m] === "function"; });
  return out;
})())`)
	if err != nil {
		t.Fatalf("reading the stub's children: %v", err)
	}
	var got struct {
		Len     int      `json:"len"`
		First   bool     `json:"first"`
		Item    bool     `json:"item"`
		Iter    int      `json:"iter"`
		Methods []string `json:"methods"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the stub's children: %v", err)
	}
	if got.Len != 2 || !got.First || !got.Item || got.Iter != 2 {
		t.Errorf("the stub's children read %+v, want a length, an index, item() and iteration", got)
	}
	if len(got.Methods) != 0 {
		t.Errorf("the stub's children carries the array methods %v, so a page that calls them passes here and throws in a browser", got.Methods)
	}
}
