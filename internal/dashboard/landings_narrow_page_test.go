package dashboard

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// The Landings table has eight columns and the page is drawn in a dashboard
// column as narrow as 470px. These tests lay the drawn table out the way a
// browser reads the page's stylesheet — columns sized from the text the cells
// hold, a 200px floor under a landing's title, a When cell that keeps one line
// — and measure what a reader would see: whether the table fits its column or
// scrolls, how many lines a cell draws, and whether text crosses a column's
// edge. They measure the layout rather than reading the rules back (gt-o7z1j).
//
// The unit tier renders nothing, so the model reads the DOM a render drew and
// the rules the page sets against it. Text is measured at the page's font: its
// stack is monospace and Menlo, the widest face it names, advances 0.6em
// (monospaceAdvance).

// landingsRow is one row of the Landings table as the page drew it.
type landingsRow struct {
	Header bool           `json:"header"`
	Cells  []landingsCell `json:"cells"`
}

// landingsCell is one cell: the classes it wears and its text, one string per
// block line the page drew — a <br> starts a new one.
type landingsCell struct {
	Class string   `json:"class"`
	Lines []string `json:"lines"`
}

// landingsTableRows reads the Landings table as the page drew it: the header row
// and every body row, in order. A render that drew no table is a fatal, not an
// empty reading.
func landingsTableRows(t *testing.T, p *pageDOM) []landingsRow {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var tbl = __find(document.getElementById("trend"), ".ltable");
  if (!tbl) return null;
  return tbl.children.map(function (tr) {
    var header = !!tr.children.length && tr.children[0].tagName === "TH";
    return {header: header, cells: tr.children.map(function (td) {
      var lines = [""];
      (function walk(n) {
        if (n.tagName === "BR") { lines.push(""); return; }
        if (!n.children || !n.children.length) { lines[lines.length - 1] += String(n.textContent || ""); return; }
        for (var i = 0; i < n.children.length; i++) walk(n.children[i]);
      })(td);
      return {class: String(td.className || ""), lines: lines};
    })};
  });
})())`)
	if err != nil {
		t.Fatalf("reading the landings table: %v", err)
	}
	if v.String() == "null" {
		t.Fatal("the landings pane drew no table")
	}
	var rows []landingsRow
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the landings table: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("the landings table drew %d rows, want a header and at least one body row", len(rows))
	}
	if !rows[0].Header {
		t.Fatal("the landings table's first row is not its header")
	}
	for _, row := range rows[1:] {
		if len(row.Cells) != len(rows[0].Cells) {
			t.Fatalf("a landings row draws %d cells, want the header's %d", len(row.Cells), len(rows[0].Cells))
		}
	}
	return rows
}

// landingsColumnIndex is the index of the column whose header reads name. A
// column a test names but the table does not draw fails here.
func landingsColumnIndex(t *testing.T, rows []landingsRow, name string) int {
	t.Helper()

	for i, cell := range rows[0].Cells {
		if strings.TrimSpace(strings.Join(cell.Lines, " ")) == name {
			return i
		}
	}
	t.Fatalf("the landings table draws no %q column", name)
	return -1
}

// landingsCellStyle is the cascade a cell draws with: the declarations its tag
// sets, then each of its classes' rules in order, later ones winning. The page
// carries no inline style on a cell, so these are the declarations a browser
// applies.
func landingsCellStyle(t *testing.T, tag, classes string) map[string]string {
	t.Helper()

	style := map[string]string{}
	for _, decl := range strings.Split(topRule(t, tag), ";") {
		if prop, val, ok := strings.Cut(decl, ":"); ok {
			style[strings.TrimSpace(prop)] = strings.TrimSpace(val)
		}
	}
	for _, class := range strings.Fields(classes) {
		for prop, val := range cssDecls(t, class) {
			style[prop] = val
		}
	}
	return style
}

// landingsPaddingSide reads one side of a cell's padding, expanding the
// shorthand as CSS writes it, so a class that sets only padding-left keeps the
// other three sides its tag declared.
func landingsPaddingSide(t *testing.T, style map[string]string, side string) float64 {
	t.Helper()

	if v, ok := style["padding-"+side]; ok {
		return cssPxValue(t, v)
	}
	v, ok := style["padding"]
	if !ok {
		t.Fatalf("a landings cell declares no padding-%s", side)
	}
	fields := strings.Fields(v)
	switch len(fields) {
	case 1:
		fields = []string{fields[0], fields[0], fields[0], fields[0]}
	case 2:
		fields = []string{fields[0], fields[1], fields[0], fields[1]}
	case 3:
		fields = []string{fields[0], fields[1], fields[2], fields[1]}
	case 4:
	default:
		t.Fatalf("padding %q is not a CSS shorthand", v)
	}
	return cssPxValue(t, fields[map[string]int{"top": 0, "right": 1, "bottom": 2, "left": 3}[side]])
}

// landingsText is the style a cell draws its text with: the properties that
// decide where a line may break, and the size the text is measured at.
type landingsText struct {
	whiteSpace string
	wrap       string
	font       float64
}

// landingsCellText is the style a cell of the Landings table draws at: the
// page's font for a body cell, the header's smaller one, then whatever the
// cell's classes declare.
func landingsCellText(t *testing.T, tag, classes string) landingsText {
	t.Helper()

	return landingsTextOf(t, landingsCellStyle(t, tag, classes))
}

// landingsTextOf reads a cell's text style from the declarations its tag and
// classes cascade to.
func landingsTextOf(t *testing.T, style map[string]string) landingsText {
	t.Helper()

	s := landingsText{
		whiteSpace: "normal",
		wrap:       "normal",
		font:       cssPxValue(t, declOf(t, topRule(t, "html,body"), "font")),
	}
	if v, ok := style["font-size"]; ok {
		s.font = cssPxValue(t, v)
	}
	if v, ok := style["white-space"]; ok {
		s.whiteSpace = v
	}
	if v, ok := style["overflow-wrap"]; ok {
		s.wrap = v
	}
	return s
}

// landingsTextWidth is the width a run of text draws at a font size: every rune
// takes monospaceAdvance of the font's em.
func landingsTextWidth(text string, font float64) float64 {
	return float64(len([]rune(text))) * font * monospaceAdvance
}

// landingsCellWidths is the narrowest and the widest a cell can draw: the
// widest run its text cannot break at, and the widest line it can keep whole.
// A nowrap cell cannot break at all. An anywhere cell can break between any two
// characters, which is what lets its column collapse and cuts a long word
// between letters; break-word leaves the longest word whole, so the column
// keeps the room that word needs. The cell's padding and any min-width it
// declares are part of the column's width.
func landingsCellWidths(t *testing.T, tag string, cell landingsCell) (min, pref float64) {
	t.Helper()

	decls := landingsCellStyle(t, tag, cell.Class)
	style := landingsTextOf(t, decls)
	for _, line := range cell.Lines {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		pref = math.Max(pref, landingsTextWidth(text, style.font))
		switch {
		case style.whiteSpace == "nowrap":
			min = math.Max(min, landingsTextWidth(text, style.font))
		case style.wrap == "anywhere":
			min = math.Max(min, landingsTextWidth("x", style.font))
		default:
			for _, word := range strings.Fields(text) {
				min = math.Max(min, landingsTextWidth(word, style.font))
			}
		}
	}
	for _, class := range strings.Fields(cell.Class) {
		if v, ok := cssDecls(t, class)["min-width"]; ok {
			min = math.Max(min, cssPxValue(t, v))
		}
	}
	left := landingsPaddingSide(t, decls, "left")
	right := landingsPaddingSide(t, decls, "right")
	return min + left + right, pref + left + right
}

// landingsColumn is one column of the drawn table: the narrowest width its
// cells can take and the width its widest line wants.
type landingsColumn struct {
	min, pref float64
}

// landingsColumns measures each column of the drawn table from the cells the
// page drew, the header included.
func landingsColumns(t *testing.T, rows []landingsRow) []landingsColumn {
	t.Helper()

	cols := make([]landingsColumn, len(rows[0].Cells))
	for _, row := range rows {
		tag := "td"
		if row.Header {
			tag = "th"
		}
		for i, cell := range row.Cells {
			min, pref := landingsCellWidths(t, tag, cell)
			cols[i].min = math.Max(cols[i].min, min)
			cols[i].pref = math.Max(cols[i].pref, pref)
		}
	}
	return cols
}

// landingsSum totals one measurement over every column.
func landingsSum(cols []landingsColumn, f func(landingsColumn) float64) float64 {
	var total float64
	for _, c := range cols {
		total += f(c)
	}
	return total
}

// landingsLayout gives each column its width in a box of avail pixels, the way
// a browser lays out the page's auto table: the table takes the box's width,
// and where its columns cannot shrink enough it keeps their minimum widths and
// overflows the box. Above that each column takes its minimum plus a share of
// the room, in proportion to what it still wants — or an equal share where no
// column wants more than its minimum.
func landingsLayout(cols []landingsColumn, avail float64) []float64 {
	extra := avail - landingsSum(cols, func(c landingsColumn) float64 { return c.min })
	room := landingsSum(cols, func(c landingsColumn) float64 { return c.pref - c.min })
	widths := make([]float64, len(cols))
	for i, c := range cols {
		switch {
		case extra <= 0:
			widths[i] = c.min
		case room <= 0:
			widths[i] = c.min + extra/float64(len(cols))
		default:
			widths[i] = c.min + extra*(c.pref-c.min)/room
		}
	}
	return widths
}

// landingsCellBox is the width a cell's text has to draw in: its column less
// its left and right padding.
func landingsCellBox(t *testing.T, tag, classes string, column float64) float64 {
	t.Helper()

	style := landingsCellStyle(t, tag, classes)
	return column - landingsPaddingSide(t, style, "left") - landingsPaddingSide(t, style, "right")
}

// landingsPanelInset is what the section's border and its .body's padding take
// from the width the grid gives the section, before the table.
func landingsPanelInset(t *testing.T) float64 {
	t.Helper()

	panel := topRule(t, "section")
	pad := topRule(t, ".body")
	return 2*cssPxValue(t, declOf(t, panel, "border")) +
		boxSide(t, pad, "padding", "left") + boxSide(t, pad, "padding", "right")
}

// landingsNarrowPanel is the width the table gets in the 470px dashboard column
// the narrow layout was reported at.
func landingsNarrowPanel(t *testing.T) float64 {
	t.Helper()

	return 470 - landingsPanelInset(t)
}

// landingsWidePanel is the width the table gets in the dashboard's narrower
// column: the .grid's smaller track at body's 1500px cap, less the section's
// border and its .body's padding. The pane draws in whichever track its
// auto-placement lands it in, so the tighter track is the one the wide layout
// has to fit; a table that fits there fits the wider column too.
func landingsWidePanel(t *testing.T) float64 {
	t.Helper()

	body := topRule(t, "body")
	content := cssPxValue(t, declOf(t, body, "max-width")) -
		boxSide(t, body, "padding", "left") - boxSide(t, body, "padding", "right")
	grid := topRule(t, ".grid")
	tracks := frTracks(t, declOf(t, grid, "grid-template-columns"))
	left := (content - cssPxValue(t, declOf(t, grid, "gap"))) * tracks[0] / (tracks[0] + tracks[1])
	return left - landingsPanelInset(t)
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

// landingsRecentTitled returns a reading with a title on its finished landing,
// the cell whose wrapping the narrow column changes.
func landingsRecentTitled(now time.Time, title string) map[string]any {
	st := landingsQueuedRunningAndLanded(now)
	recent := st["trend"].(map[string]any)["recent"].([]any)
	recent[len(recent)-1].(map[string]any)["title"] = title
	return st
}

// At the 470px column the drawn table needs more room than the column gives it,
// so its box must scroll sideways: the om and Ship columns sit past the right
// edge, and the fade is the cue that more is there. What scrolls is measured
// from the cells the page drew, not read back from a declaration.
func TestLandingsTableScrollsSidewaysAtANarrowWidth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsReading(now))

	cols := landingsColumns(t, landingsTableRows(t, p))
	panel := landingsNarrowPanel(t)
	needed := landingsSum(cols, func(c landingsColumn) float64 { return c.min })
	if needed <= panel {
		t.Errorf("the drawn table fits %gpx at its narrowest within the %gpx column, so nothing overflows and this test measures no sideways scroll", needed, panel)
	}

	decls := cssDecls(t, "lrows")
	if got := decls["overflow-x"]; got != "auto" {
		t.Errorf("the .lrows rule has overflow-x %q, want auto so the %gpx the table needs past the column is reachable", got, needed-panel)
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
		t.Error("the landings table is not drawn in the .lrows scroll box, so the width it needs past the column is unreachable")
	}
}

// A When cell reads as one value: in the narrow column the drawn text fits its
// column on one line, for the queued, running and finished rows alike. A grid
// of the table at 470px used to split a running row's "merging 47s" over two
// lines (gt-u7xe9).
func TestLandingsWhenCellNeverWraps(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsQueuedRunningAndLanded(now))

	rows := landingsTableRows(t, p)
	when := landingsColumnIndex(t, rows, "When")
	widths := landingsLayout(landingsColumns(t, rows), landingsNarrowPanel(t))

	merging := false
	for _, row := range rows[1:] {
		cell := row.Cells[when]
		if !hasClassToken(cell.Class, "when") {
			t.Errorf("the %q cell carries class %q, want the when class the nowrap rule names", strings.Join(cell.Lines, " "), cell.Class)
		}
		style := landingsCellText(t, "td", cell.Class)
		box := landingsCellBox(t, "td", cell.Class, widths[when])
		text := strings.Join(nonEmptyLines(cell.Lines), " ")
		if w := landingsTextWidth(text, style.font); w > box {
			t.Errorf("the When cell %q draws %gpx of text in the %gpx its column leaves, so the reading spills past the cell", text, w, box)
		}
		if style.whiteSpace != "nowrap" {
			if lines := mangoLines(text, mangoMeasure{wrap: style.wrap, font: style.font}, box); lines != 1 {
				t.Errorf("the When cell %q draws on %d lines in its %gpx column, want one line: it reads as one value", text, lines, box)
			}
		}
		if strings.HasPrefix(text, "merging ") {
			merging = true
		}
	}
	if !merging {
		t.Fatal("no row draws a merging reading, so the running row's When cell was not measured")
	}
}

// The Landing title keeps a readable width: in the narrow column its column is
// at least 200px wide and every word draws whole inside it, so a title is never
// a strip of one- and two-letter lines (gt-u7xe9).
func TestLandingsTitleCellKeepsAReadableWidth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	const title = "fix: the landings table scrolls sideways in a narrow dashboard column"
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsRecentTitled(now, title))

	rows := landingsTableRows(t, p)
	landing := landingsColumnIndex(t, rows, "Landing")
	widths := landingsLayout(landingsColumns(t, rows), landingsNarrowPanel(t))

	titled := false
	for _, row := range rows[1:] {
		cell := row.Cells[landing]
		if !hasClassToken(cell.Class, "main") {
			t.Errorf("the Landing cell of row %q carries class %q, want the main class the title rule names", strings.Join(row.Cells[0].Lines, " "), cell.Class)
		}
		box := landingsCellBox(t, "td", cell.Class, widths[landing])
		if box < 200 {
			t.Errorf("the Landing column leaves %gpx for a title in the narrow column, want the 200px floor", box)
		}
		style := landingsCellText(t, "td", cell.Class)
		for _, line := range cell.Lines {
			for _, word := range strings.Fields(line) {
				if w := landingsTextWidth(word, style.font); w > box {
					t.Errorf("the word %q needs %gpx, wider than the %gpx the Landing column leaves: the title would break mid-word", word, w, box)
				}
			}
			if strings.TrimSpace(line) == title {
				titled = true
			}
		}
	}
	if !titled {
		t.Fatalf("the table draws no row whose title reads %q", title)
	}
}

// A title can carry one long unbroken token — a path, a digest. The Landing
// column keeps the room the token needs, so the token stays whole on one line
// and the table scrolls further, instead of the column collapsing to its floor
// and cutting the token between letters (gt-o7z1j).
func TestLandingsLongTokenKeepsItsColumn(t *testing.T) {
	t.Parallel()

	const token = "sha256:6f1c0b8e2a4d9f3c5b7e1a2d4c6f8b0e3a5c7e9d1f3a5c7e9b1d3f5a7c9e1b3d"
	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsRecentTitled(now, "landed "+token))

	rows := landingsTableRows(t, p)
	cols := landingsColumns(t, rows)
	landing := landingsColumnIndex(t, rows, "Landing")
	panel := landingsNarrowPanel(t)
	widths := landingsLayout(cols, panel)

	if needed := landingsSum(cols, func(c landingsColumn) float64 { return c.min }); needed <= panel {
		t.Errorf("the drawn table fits %gpx at its narrowest within the %gpx column, so nothing overflows and this test measures no sideways scroll", needed, panel)
	}

	found := false
	for _, row := range rows[1:] {
		cell := row.Cells[landing]
		style := landingsCellText(t, "td", cell.Class)
		box := landingsCellBox(t, "td", cell.Class, widths[landing])
		for _, line := range cell.Lines {
			if !strings.Contains(line, token) {
				continue
			}
			found = true
			if w := landingsTextWidth(token, style.font); w > box {
				t.Errorf("the token needs %gpx and the Landing column leaves %gpx: the column collapsed and the token would be cut between letters", w, box)
			}
			if lines := mangoLines(token, mangoMeasure{wrap: style.wrap, font: style.font}, box); lines != 1 {
				t.Errorf("the token draws on %d lines in its %gpx column, want one: a long word is never split where a column can keep it whole", lines, box)
			}
		}
	}
	if !found {
		t.Fatalf("the table draws no row carrying the token %q", token)
	}
}

// The wide layout keeps the drawn table inside its column: at the width the
// dashboard's wider column gives it, the columns' minimum widths fit, so
// nothing scrolls; the Landing column is wider than its 200px floor because its
// title's own text asks for more, so the floor does not reshape the wide layout
// (gt-o7z1j).
func TestLandingsTableKeepsTheWideLayout(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	const title = "fix: the landings table scrolls sideways in a narrow dashboard column"
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsRecentTitled(now, title))

	rows := landingsTableRows(t, p)
	cols := landingsColumns(t, rows)
	panel := landingsWidePanel(t)
	if needed := landingsSum(cols, func(c landingsColumn) float64 { return c.min }); needed > panel {
		t.Errorf("the drawn table needs %gpx at its narrowest, more than the %gpx the wide column gives it: the wide layout would scroll", needed, panel)
	}
	widths := landingsLayout(cols, panel)

	landing := landingsColumnIndex(t, rows, "Landing")
	titled := false
	for _, row := range rows[1:] {
		cell := row.Cells[landing]
		box := landingsCellBox(t, "td", cell.Class, widths[landing])
		if box <= 200 {
			t.Errorf("the Landing column leaves %gpx for a title in the wide layout, want the title's own text to take more than the 200px floor", box)
		}
		style := landingsCellText(t, "td", cell.Class)
		for _, line := range cell.Lines {
			for _, word := range strings.Fields(line) {
				if w := landingsTextWidth(word, style.font); w > box {
					t.Errorf("the wide layout draws the word %q in %gpx, wider than the %gpx its column leaves: text would cross the column's edge", word, w, box)
				}
			}
			if strings.TrimSpace(line) == title {
				titled = true
			}
		}
	}
	if !titled {
		t.Fatalf("the table draws no row whose title reads %q", title)
	}
}

// nonEmptyLines returns the cell's block lines that hold text, so a cell's <br>
// separators do not read as empty words.
func nonEmptyLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
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
