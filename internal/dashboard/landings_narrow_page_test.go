package dashboard

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Landings table draws eight columns of landings into a dashboard column.
// These tests read the table a render drew and the rules the page sets, and
// compare the table's minimum width — the sum of its columns' minimum widths,
// the width an auto table never draws narrower than — with the width the page
// leaves it. Three things follow from that minimum: whether the scroll box
// overflows, how narrow the Landing column can get, and whether a long token
// fits whole. The unit tier has no layout engine and these tests do not
// simulate one: a column's minimum is the widest run of its text that cannot
// break, or the min-width its cells declare, plus their padding. The table is
// width:100%, so it overflows its box exactly when that minimum exceeds the box.
//
// Text is measured at landingsAdvance per character, the advance of the widest
// face the page's font stack names.

// landingsAdvance is one monospace character in ems. The page's stack names
// Menlo, which advances 602/1000 of its em, wider than the stack's other named
// faces.
const landingsAdvance = 0.6023

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

// landingsBlock is one block of the page's stylesheet: a rule, with its
// selector and declarations, or an at-rule, with its condition and the blocks it
// wraps. The page writes a rule's declarations with no nested blocks.
type landingsBlock struct {
	sel    string
	decls  string
	nested []landingsBlock
}

// landingsSheet parses css into its blocks.
func landingsSheet(t *testing.T, css string) []landingsBlock {
	t.Helper()

	var blocks []landingsBlock
	for i := 0; i < len(css); {
		if strings.ContainsRune(" \t\r\n;", rune(css[i])) {
			i++
			continue
		}
		open := strings.IndexByte(css[i:], '{')
		if open < 0 {
			if rest := strings.TrimSpace(css[i:]); rest != "" {
				t.Fatalf("the page's stylesheet ends in %q, not a block", rest)
			}
			return blocks
		}
		at := i + open
		sel := strings.TrimSpace(css[i:at])
		body, width, ok := landingsBraced(css[at:])
		if !ok {
			t.Fatalf("the stylesheet's %q block has no closing brace", sel)
		}
		if strings.HasPrefix(sel, "@") {
			blocks = append(blocks, landingsBlock{sel: sel, nested: landingsSheet(t, body)})
		} else {
			blocks = append(blocks, landingsBlock{sel: sel, decls: body})
		}
		i = at + width
	}
	return blocks
}

// landingsBraced reads the block that starts at s[0], a "{", returning what it
// wraps and how many bytes it takes, closing brace included.
func landingsBraced(s string) (body string, width int, ok bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[1:i], i + 1, true
			}
		}
	}
	return "", 0, false
}

// landingsCSS is index.html's stylesheet with its comments removed, so a rule
// reads without a comment's braces or semicolons.
func landingsCSS(t *testing.T) string {
	t.Helper()

	page := string(indexHTML)
	open := strings.Index(page, "<style>")
	if open < 0 {
		t.Fatal("index.html has no style block")
	}
	closed := strings.Index(page[open:], "</style>")
	if closed < 0 {
		t.Fatal("index.html's style block never closes")
	}
	css := page[open+len("<style>") : open+closed]
	for {
		start := strings.Index(css, "/*")
		if start < 0 {
			return css
		}
		end := strings.Index(css[start:], "*/")
		if end < 0 {
			t.Fatal("the page's stylesheet has an unterminated comment")
		}
		css = css[:start] + css[start+end+2:]
	}
}

// landingsTopLevel returns the stylesheet's rules, leaving out the rules nested
// in at-rules: a rule inside @media is another viewport's layout, and a test
// that wants one reads it through landingsStackedAt.
func landingsTopLevel(t *testing.T) []landingsBlock {
	t.Helper()

	var rules []landingsBlock
	for _, block := range landingsSheet(t, landingsCSS(t)) {
		if !strings.HasPrefix(block.sel, "@") {
			rules = append(rules, block)
		}
	}
	return rules
}

// landingsRule returns the declarations of the last top-level rule whose
// selector is sel, as the cascade serves the last of two equal rules. A rule the
// page must set and does not is a fatal.
func landingsRule(t *testing.T, sel string) string {
	t.Helper()

	found := ""
	for _, rule := range landingsTopLevel(t) {
		if rule.sel == sel {
			found = rule.decls
		}
	}
	if found == "" {
		t.Fatalf("index.html has no %s rule", sel)
	}
	return found
}

// landingsClassDecls returns the declarations the top-level rules give a class
// as their subject, later rules winning. A rule styles a class when its last
// compound names it: .ltable td.main styles the main cell, and .mainline only
// names a different class.
func landingsClassDecls(t *testing.T, class string) map[string]string {
	t.Helper()

	decls := map[string]string{}
	for _, rule := range landingsTopLevel(t) {
		if !landingsSelectsClass(rule.sel, class) {
			continue
		}
		for _, decl := range strings.Split(rule.decls, ";") {
			prop, val, ok := strings.Cut(decl, ":")
			if ok {
				decls[strings.TrimSpace(prop)] = strings.TrimSpace(val)
			}
		}
	}
	return decls
}

// landingsSelectsClass reports whether a selector's subject — its last compound
// — names class as a whole class.
func landingsSelectsClass(sel, class string) bool {
	for _, one := range strings.Split(sel, ",") {
		fields := strings.Fields(one)
		if len(fields) == 0 {
			continue
		}
		for _, name := range strings.Split(fields[len(fields)-1], ".") {
			if name == class {
				return true
			}
		}
	}
	return false
}

// landingsDecl returns one property of a rule's declarations.
func landingsDecl(t *testing.T, decls, prop string) string {
	t.Helper()

	for _, decl := range strings.Split(decls, ";") {
		if k, v, ok := strings.Cut(decl, ":"); ok && strings.TrimSpace(k) == prop {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no %s in %q", prop, decls)
	return ""
}

// landingsPx reads a declaration's leading pixel measurement: "1500px" is 1500,
// "1px solid var(--line)" is 1, and a unitless "0" is 0 as CSS reads it.
func landingsPx(t *testing.T, v string) float64 {
	t.Helper()

	if strings.TrimSpace(v) == "0" {
		return 0
	}
	end := strings.IndexFunc(v, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if end <= 0 || !strings.HasPrefix(v[end:], "px") {
		t.Fatalf("%q is not a pixel measurement", v)
	}
	n, err := strconv.ParseFloat(v[:end], 64)
	if err != nil {
		t.Fatalf("%q is not a pixel measurement: %v", v, err)
	}
	return n
}

// landingsBoxSide returns one side of a box's padding or border: a longhand
// declaration wins over the shorthand, which expands as CSS writes it.
func landingsBoxSide(t *testing.T, decls, prop, side string) float64 {
	t.Helper()

	if strings.Contains(decls, prop+"-"+side+":") {
		return landingsPx(t, landingsDecl(t, decls, prop+"-"+side))
	}
	fields := strings.Fields(landingsDecl(t, decls, prop))
	switch len(fields) {
	case 1:
		return landingsPx(t, fields[0])
	case 2:
		fields = []string{fields[0], fields[1], fields[0], fields[1]}
	case 3:
		fields = []string{fields[0], fields[1], fields[2], fields[1]}
	case 4:
	default:
		t.Fatalf("%s %q is not a CSS shorthand", prop, strings.Join(fields, " "))
	}
	return landingsPx(t, fields[map[string]int{"top": 0, "right": 1, "bottom": 2, "left": 3}[side]])
}

// landingsTrackShares reads the share of each track in a grid-template-columns
// value: minmax(0,1.25fr) is 1.25.
func landingsTrackShares(t *testing.T, template string) []float64 {
	t.Helper()

	var shares []float64
	for i := 0; i < len(template); i++ {
		if !strings.HasPrefix(template[i:], "fr") {
			continue
		}
		start := i
		for start > 0 && (template[start-1] >= '0' && template[start-1] <= '9' || template[start-1] == '.') {
			start--
		}
		share, err := strconv.ParseFloat(template[start:i], 64)
		if err != nil {
			t.Fatalf("reading the tracks of %q: %v", template, err)
		}
		shares = append(shares, share)
	}
	if len(shares) == 0 {
		t.Fatalf("grid-template-columns %q declares no track", template)
	}
	return shares
}

// landingsStackedAt is the widest page at which .grid draws one column: the
// max-width of the media block whose .grid rule replaces the two tracks. A
// stylesheet without that rule is a fatal, since the narrow layout depends on it.
func landingsStackedAt(t *testing.T) float64 {
	t.Helper()

	for _, block := range landingsSheet(t, landingsCSS(t)) {
		cond, ok := strings.CutPrefix(block.sel, "@media")
		if !ok {
			continue
		}
		value, ok := strings.CutPrefix(strings.TrimSpace(cond), "(max-width:")
		if !ok {
			continue
		}
		grid := ""
		for _, rule := range block.nested {
			if rule.sel == ".grid" {
				grid = rule.decls
			}
		}
		if !strings.Contains(grid, "grid-template-columns") {
			continue
		}
		return landingsPx(t, strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), ")")))
	}
	t.Fatal("index.html has no @media block that restacks .grid into one column")
	return 0
}

// landingsGridColumns is the share of each track the .grid draws. The Landings
// section takes one of them, so the tightest track is the least width it can be
// drawn in.
func landingsGridColumns(t *testing.T) []float64 {
	t.Helper()

	return landingsTrackShares(t, landingsDecl(t, landingsRule(t, ".grid"), "grid-template-columns"))
}

// landingsPanelInset is what the section's border and its .body's padding take
// from the width the .grid gives the section.
func landingsPanelInset(t *testing.T) float64 {
	t.Helper()

	panel := landingsRule(t, "section")
	pad := landingsRule(t, ".body")
	return 2*landingsPx(t, landingsDecl(t, panel, "border")) +
		landingsBoxSide(t, pad, "padding", "left") + landingsBoxSide(t, pad, "padding", "right")
}

// landingsPanelWidth is the width the Landings table's scroll box gets on a page
// pageWidth pixels wide: the body's content width — the page, capped at the
// body's max-width, less the body's own horizontal padding — split into the
// .grid's tracks, less landingsPanelInset. At or below the width the stylesheet
// stacks the grid at, each column takes the whole content width.
func landingsPanelWidth(t *testing.T, pageWidth float64) float64 {
	t.Helper()

	body := landingsRule(t, "body")
	content := math.Min(pageWidth, landingsPx(t, landingsDecl(t, body, "max-width"))) -
		landingsBoxSide(t, body, "padding", "left") -
		landingsBoxSide(t, body, "padding", "right")
	column := content
	if pageWidth > landingsStackedAt(t) {
		shares := landingsGridColumns(t)
		tightest := shares[0]
		sum := 0.0
		for _, share := range shares {
			tightest = math.Min(tightest, share)
			sum += share
		}
		grid := landingsRule(t, ".grid")
		column = (content - landingsPx(t, landingsDecl(t, grid, "gap"))) * tightest / sum
	}
	return column - landingsPanelInset(t)
}

// landingsNarrowColumn is the dashboard column width the narrow layout was
// reported at.
const landingsNarrowColumn = 470

// landingsNarrowPanel is the width the table gets in that column. The page is
// the column plus the body's horizontal padding, and the .grid stacks at that
// page width, so the column is the body's whole content width: the narrow panel
// goes through the same derivation as the wide one rather than treating the
// column width as if it were a page width.
func landingsNarrowPanel(t *testing.T) float64 {
	t.Helper()

	body := landingsRule(t, "body")
	pageWidth := landingsNarrowColumn +
		landingsBoxSide(t, body, "padding", "left") +
		landingsBoxSide(t, body, "padding", "right")
	if pageWidth > landingsStackedAt(t) {
		t.Fatalf("a %gpx page splits the .grid into two tracks, so it has no %dpx column", pageWidth, landingsNarrowColumn)
	}
	panel := landingsPanelWidth(t, pageWidth)
	if got := panel + landingsPanelInset(t); math.Abs(got-landingsNarrowColumn) > 1e-6 {
		t.Fatalf("a %gpx page leaves the Landings column %gpx, not the %dpx the report names", pageWidth, got, landingsNarrowColumn)
	}
	return panel
}

// landingsWidePanel is the width the table gets at the page's widest: the body's
// max-width, split into the .grid's tracks, at the tightest track the Landings
// section can take.
func landingsWidePanel(t *testing.T) float64 {
	t.Helper()

	body := landingsRule(t, "body")
	return landingsPanelWidth(t, landingsPx(t, landingsDecl(t, body, "max-width")))
}

// landingsCellStyle is the cascade a cell draws with: the declarations its tag
// sets, then each of its classes' rules in order, later ones winning. The page
// carries no inline style on a cell, so these are the declarations a browser
// applies.
func landingsCellStyle(t *testing.T, tag, classes string) map[string]string {
	t.Helper()

	style := map[string]string{}
	for _, decl := range strings.Split(landingsRule(t, tag), ";") {
		if prop, val, ok := strings.Cut(decl, ":"); ok {
			style[strings.TrimSpace(prop)] = strings.TrimSpace(val)
		}
	}
	for _, class := range strings.Fields(classes) {
		for prop, val := range landingsClassDecls(t, class) {
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
		return landingsPx(t, v)
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
	return landingsPx(t, fields[map[string]int{"top": 0, "right": 1, "bottom": 2, "left": 3}[side]])
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
		font:       landingsPx(t, landingsDecl(t, landingsRule(t, "html,body"), "font")),
	}
	if v, ok := style["font-size"]; ok {
		s.font = landingsPx(t, v)
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
// takes landingsAdvance of the font's em.
func landingsTextWidth(text string, font float64) float64 {
	return float64(len([]rune(text))) * font * landingsAdvance
}

// landingsFits reports whether width draws inside box. A cell's box is measured
// by subtracting its padding from a column whose minimum was measured by adding
// it, so the two sides agree only up to floating-point rounding, and a wider
// column only gives the cell more room.
func landingsFits(width, box float64) bool {
	return width <= box+1e-6
}

// landingsCellMinWidth is the narrowest a cell can draw: the widest run of its
// text that cannot break, or the min-width it declares, plus its padding.
//
// A nowrap cell cannot break at all. An anywhere cell can break between any two
// characters, and CSS counts those breaks when it sizes the column, so the
// column collapses to one character. break-word does not affect intrinsic
// sizing: its cells break a word only when the line is already narrower than the
// word, which an auto table's column never lets happen, so its minimum stays the
// longest word. Every other cell breaks between words.
func landingsCellMinWidth(t *testing.T, tag string, cell landingsCell) float64 {
	t.Helper()

	decls := landingsCellStyle(t, tag, cell.Class)
	style := landingsTextOf(t, decls)
	var min float64
	for _, line := range cell.Lines {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
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
		if v, ok := landingsClassDecls(t, class)["min-width"]; ok {
			min = math.Max(min, landingsPx(t, v))
		}
	}
	return min + landingsPaddingSide(t, decls, "left") + landingsPaddingSide(t, decls, "right")
}

// landingsColumnMins measures each column of the drawn table at its narrowest,
// from the cells the page drew, the header included.
func landingsColumnMins(t *testing.T, rows []landingsRow) []float64 {
	t.Helper()

	cols := make([]float64, len(rows[0].Cells))
	for _, row := range rows {
		tag := "td"
		if row.Header {
			tag = "th"
		}
		for i, cell := range row.Cells {
			cols[i] = math.Max(cols[i], landingsCellMinWidth(t, tag, cell))
		}
	}
	return cols
}

// landingsTotalMin totals the columns' minimum widths: the table's minimum.
func landingsTotalMin(cols []float64) float64 {
	var total float64
	for _, col := range cols {
		total += col
	}
	return total
}

// landingsCellBox is the width a cell's text has to draw in: its column less its
// left and right padding. At the narrow width the columns are at their minimum,
// and a wider table only gives each column more, so this is the least room the
// cell can have.
func landingsCellBox(t *testing.T, tag, classes string, column float64) float64 {
	t.Helper()

	style := landingsCellStyle(t, tag, classes)
	return column - landingsPaddingSide(t, style, "left") - landingsPaddingSide(t, style, "right")
}

// landingsLayers splits a comma-separated property value into its layers.
func landingsLayers(val string) []string {
	parts := strings.Split(val, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
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
// the cell whose wrapping the narrow column must keep readable.
func landingsRecentTitled(now time.Time, title string) map[string]any {
	st := landingsQueuedRunningAndLanded(now)
	recent := st["trend"].(map[string]any)["recent"].([]any)
	recent[len(recent)-1].(map[string]any)["title"] = title
	return st
}

// At the narrow column the drawn table's columns add up to more than the panel
// leaves, so its box must scroll sideways: the last columns sit past the right
// edge, and the fade is the cue that more is there. What scrolls is measured
// from the cells the page drew, not read back from a declaration.
func TestLandingsTableScrollsSidewaysAtANarrowWidth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsReading(now))

	rows := landingsTableRows(t, p)
	panel := landingsNarrowPanel(t)
	needed := landingsTotalMin(landingsColumnMins(t, rows))
	if needed <= panel {
		t.Errorf("the drawn table fits %gpx at its narrowest within the %gpx column, so nothing overflows and this test measures no sideways scroll", needed, panel)
	}

	decls := landingsClassDecls(t, "lrows")
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
	if got, want := landingsLayers(decls["background-attachment"]), []string{"local", "local", "scroll", "scroll"}; strings.Join(got, ",") != strings.Join(want, ",") {
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

// A When cell reads as one value: in the narrow column the text of every row —
// queued, running and finished alike — fits the width its column's minimum
// leaves, so it draws one line. The column's minimum includes the whole text
// only while the cell is nowrap; were the text allowed to wrap, the column would
// shrink to its longest word and a running row's "merging 47s" would take two
// lines.
func TestLandingsWhenCellNeverWraps(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsQueuedRunningAndLanded(now))

	rows := landingsTableRows(t, p)
	cols := landingsColumnMins(t, rows)
	if panel := landingsNarrowPanel(t); landingsTotalMin(cols) <= panel {
		t.Errorf("the drawn table fits the %gpx column at its narrowest, so no column is squeezed to its minimum and this test measures no narrow layout", panel)
	}

	when := landingsColumnIndex(t, rows, "When")
	merging := false
	for _, row := range rows[1:] {
		cell := row.Cells[when]
		if !hasClassToken(cell.Class, "when") {
			t.Errorf("the %q cell carries class %q, want the when class the nowrap rule names", strings.Join(cell.Lines, " "), cell.Class)
		}
		style := landingsCellText(t, "td", cell.Class)
		box := landingsCellBox(t, "td", cell.Class, cols[when])
		text := strings.Join(nonEmptyLines(cell.Lines), " ")
		if w := landingsTextWidth(text, style.font); !landingsFits(w, box) {
			t.Errorf("the When cell %q draws %gpx of text in the %gpx its column leaves, so the reading wraps instead of reading as one value", text, w, box)
		}
		if strings.HasPrefix(text, "merging ") {
			merging = true
		}
	}
	if !merging {
		t.Fatal("no row draws a merging reading, so the running row's When cell was not measured")
	}
}

// The Landing title keeps a readable width: in the narrow column the column's
// minimum is at least the 200px floor its cells declare, so a title is never
// squeezed into a strip of one- and two-letter lines, and every word of the
// title fits the width that minimum leaves.
func TestLandingsTitleCellKeepsAReadableWidth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	const title = "fix: the landings table scrolls sideways in a narrow dashboard column"
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsRecentTitled(now, title))

	rows := landingsTableRows(t, p)
	cols := landingsColumnMins(t, rows)
	if panel := landingsNarrowPanel(t); landingsTotalMin(cols) <= panel {
		t.Errorf("the drawn table fits the %gpx column at its narrowest, so no column is squeezed to its minimum and this test measures no narrow layout", panel)
	}

	landing := landingsColumnIndex(t, rows, "Landing")
	titled := false
	for _, row := range rows[1:] {
		cell := row.Cells[landing]
		if !hasClassToken(cell.Class, "main") {
			t.Errorf("the Landing cell of row %q carries class %q, want the main class the title rule names", strings.Join(row.Cells[0].Lines, " "), cell.Class)
		}
		box := landingsCellBox(t, "td", cell.Class, cols[landing])
		if box < 200 {
			t.Errorf("the Landing column leaves %gpx for a title in the narrow column, want the 200px floor", box)
		}
		style := landingsCellText(t, "td", cell.Class)
		for _, line := range cell.Lines {
			for _, word := range strings.Fields(line) {
				if w := landingsTextWidth(word, style.font); !landingsFits(w, box) {
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
// and the table scrolls further, rather than the column collapsing to its floor
// and the token breaking between letters. The cell's overflow-wrap must be
// break-word and not anywhere: break-word leaves intrinsic sizing to the longest
// word, so the column keeps the token's width, while anywhere counts a break
// between any two characters and lets the column shrink to one character.
func TestLandingsLongTokenKeepsItsColumn(t *testing.T) {
	t.Parallel()

	const token = "sha256:6f1c0b8e2a4d9f3c5b7e1a2d4c6f8b0e3a5c7e9d1f3a5c7e9b1d3f5a7c9e1b3d"
	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsRecentTitled(now, "landed "+token))

	rows := landingsTableRows(t, p)
	cols := landingsColumnMins(t, rows)
	if panel := landingsNarrowPanel(t); landingsTotalMin(cols) <= panel {
		t.Errorf("the drawn table fits the %gpx column at its narrowest, so nothing overflows and this test measures no long token in a squeezed column", panel)
	}

	landing := landingsColumnIndex(t, rows, "Landing")
	found := false
	for _, row := range rows[1:] {
		cell := row.Cells[landing]
		style := landingsCellText(t, "td", cell.Class)
		box := landingsCellBox(t, "td", cell.Class, cols[landing])
		for _, line := range cell.Lines {
			if !strings.Contains(line, token) {
				continue
			}
			found = true
			if w := landingsTextWidth(token, style.font); !landingsFits(w, box) {
				t.Errorf("the token needs %gpx and the Landing column leaves %gpx: the column collapsed and the token would be cut between letters", w, box)
			}
			if got := landingsCellStyle(t, "td", cell.Class)["overflow-wrap"]; got != "break-word" {
				t.Errorf("the Landing cell's overflow-wrap is %q, want break-word: anywhere lets the column shrink to one character", got)
			}
		}
	}
	if !found {
		t.Fatalf("the table draws no row carrying the token %q", token)
	}
}

// The wide layout keeps the drawn table inside its column: at the page's widest
// the columns' minimum widths fit the tightest track the .grid can give the
// Landings section, so nothing scrolls; the Landing column still leaves its
// 200px floor, and every word of the title fits the width it leaves.
func TestLandingsTableKeepsTheWideLayout(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	const title = "fix: the landings table scrolls sideways in a narrow dashboard column"
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsRecentTitled(now, title))

	rows := landingsTableRows(t, p)
	cols := landingsColumnMins(t, rows)
	panel := landingsWidePanel(t)
	if needed := landingsTotalMin(cols); needed > panel {
		t.Errorf("the drawn table needs %gpx at its narrowest, more than the %gpx the tightest wide column gives it: the wide layout would scroll", needed, panel)
	}

	landing := landingsColumnIndex(t, rows, "Landing")
	titled := false
	for _, row := range rows[1:] {
		cell := row.Cells[landing]
		box := landingsCellBox(t, "td", cell.Class, cols[landing])
		if box < 200 {
			t.Errorf("the Landing column leaves %gpx for a title in the wide layout, want the 200px floor", box)
		}
		style := landingsCellText(t, "td", cell.Class)
		for _, line := range cell.Lines {
			for _, word := range strings.Fields(line) {
				if w := landingsTextWidth(word, style.font); !landingsFits(w, box) {
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
