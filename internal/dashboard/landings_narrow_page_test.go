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
// its class and each cell's class and text.
type landingsBodyRow struct {
	Class string `json:"class"`
	Cells []struct {
		Class string `json:"class"`
		Text  string `json:"text"`
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
      return {class: td.className || "", text: td.textContent};
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

// hasClassToken reports whether a space-separated class attribute names class.
func hasClassToken(attr, class string) bool {
	for _, c := range strings.Fields(attr) {
		if c == class {
			return true
		}
	}
	return false
}
