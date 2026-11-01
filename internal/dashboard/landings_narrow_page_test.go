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
// it into one- and two-letter lines.
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

// hasClassToken reports whether a space-separated class attribute names class.
func hasClassToken(attr, class string) bool {
	for _, c := range strings.Fields(attr) {
		if c == class {
			return true
		}
	}
	return false
}
