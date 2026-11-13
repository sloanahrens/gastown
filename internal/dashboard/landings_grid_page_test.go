package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The Landings table draws every row on the seat table's grid, one line per
// landing: the bead's id, its priority pill and its title all sit in the Bead
// cell, and nothing a row used to print on a second line is drawn there. These
// read the drawn page back, so a row that grows a second line again fails here
// rather than in a browser (gt-ultode).

// landingsOutcomes is a reading with every outcome the table draws: a bead
// waiting to land, the landing the worker is running, one it rejected, one
// failing in backoff and one it landed. Every row carries a priority and a
// title except the running and queued ones, whose feeds have neither.
func landingsOutcomes(now time.Time) map[string]any {
	at := func(m int) string { return now.Add(-time.Duration(m) * time.Minute).Format(time.RFC3339) }
	return map[string]any{
		"queue": map[string]any{"landing": []any{map[string]any{
			"id": "gt-queued", "rig": "gastown", "title": "a bead waiting to land", "priority": 1,
			"assignee": "gastown/polecats/dag", "updated_at": at(20),
		}}},
		"trend": map[string]any{
			"hours":  []map[string]any{{"hour": now.Add(-time.Hour).Format(time.RFC3339), "landed": 1, "rejected": 1}},
			"stages": []any{},
			"recent": []any{
				map[string]any{"at": at(15), "bead": "gt-running", "rig": "gastown", "outcome": "running", "polecat": "dag"},
				map[string]any{"at": at(10), "bead": "gt-rejected", "rig": "gastown", "outcome": "rejected",
					"kind": "gate", "polecat": "dag", "priority": 2, "title": "a rejected bead",
					"detail": "the gate failed: TestTheThing"},
				map[string]any{"at": at(8), "bead": "gt-failing", "rig": "gastown", "outcome": "backoff",
					"stage": "om", "failures": 3, "polecat": "dag", "priority": 3, "title": "a failing bead",
					"detail": "the review timed out"},
				map[string]any{"at": at(5), "bead": "gt-landed", "rig": "gastown", "outcome": "landed",
					"polecat": "dag", "priority": 4, "title": "a landed bead", "agent": "deepseek-flash",
					"commit": "abc123", "score": 0.86, "gate_secs": 42.0, "om_secs": 61.0, "ship_secs": 180.0},
			},
		},
	}
}

// oneLineRow is one drawn landings row as these tests read it: how many cells it
// drew, whether anything inside it is a <br>, and the Bead cell's parts.
type oneLineRow struct {
	Cells int    `json:"cells"`
	BRs   int    `json:"brs"`
	ID    string `json:"id"`
	Pill  string `json:"pill"`
	Title string `json:"title"`
}

// oneLineRows reads every drawn landings row back by its Bead cell.
func oneLineRows(t *testing.T, p *pageDOM) []oneLineRow {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var tbl = __find(document.getElementById("trend"), ".ltable");
  if (!tbl) return [];
  function part(e) { return e ? e.textContent : ""; }
  return tbl.children.__arr.slice(1).map(function (tr) {
    var bead = tr.children[3], brs = 0;
    __walk(tr, function (c) { if (c.tagName === "BR") brs++; });
    return {cells: tr.children.length, brs: brs, id: part(__find(bead, ".bead")),
            pill: part(__find(bead, ".tag")) || part(__find(bead, ".pnone")),
            title: part(__find(bead, ".ltitle"))};
  });
})())`)
	if err != nil {
		t.Fatalf("reading the drawn landings rows: %v", err)
	}
	var rows []oneLineRow
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the drawn landings rows: %v", err)
	}
	return rows
}

// A landings row is one line whatever the outcome: queued, running, rejected,
// failing and landed rows all draw the Bead cell as the id, the priority pill
// (or the dash a row the feed gives no priority draws) and the title, and no
// row holds a <br> for a second line (gt-ultode).
func TestLandingsRowsDrawOneLinePerOutcome(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsOutcomes(now))

	rows := oneLineRows(t, p)
	if len(rows) != 5 {
		t.Fatalf("the table drew %d rows, want the queued, running, rejected, failing and landed ones", len(rows))
	}
	want := []oneLineRow{
		{Cells: 10, ID: "gt-queued", Pill: "P1", Title: "gastown · a bead waiting to land"},
		{Cells: 10, ID: "gt-running", Pill: "–"},
		{Cells: 10, ID: "gt-rejected", Pill: "P2", Title: "a rejected bead"},
		{Cells: 10, ID: "gt-failing", Pill: "P3", Title: "a failing bead"},
		{Cells: 10, ID: "gt-landed", Pill: "P4", Title: "a landed bead"},
	}
	for i, w := range want {
		if rows[i].Cells != w.Cells {
			t.Errorf("the %s row drew %d cells, want the table's %d columns", w.ID, rows[i].Cells, w.Cells)
		}
		if rows[i].BRs != 0 {
			t.Errorf("the %s row holds %d <br>, want one line per row", w.ID, rows[i].BRs)
		}
		if rows[i].ID != w.ID {
			t.Errorf("row %d's bead cell reads %q, want the bead id %q", i, rows[i].ID, w.ID)
		}
		if rows[i].Pill != w.Pill {
			t.Errorf("the %s row's priority reads %q, want %q", w.ID, rows[i].Pill, w.Pill)
		}
		if rows[i].Title != w.Title {
			t.Errorf("the %s row's title reads %q, want %q", w.ID, rows[i].Title, w.Title)
		}
	}
}

// The two tables read as one format: the Polecats table heads Polecat, Model,
// Bead, State and Age, the Landings table heads When, then the same Polecat,
// Model, Bead and State columns, then Polecat time, Score, Gate, om and Ship;
// and every drawn row fills exactly the columns its table heads (gt-ultode).
func TestTheLandingsTableHeadsTheSeatTablesColumns(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	st := landingsOutcomes(now)
	st["polecats"] = []any{
		seat("gastown", "dag", "working", "gt-held", "a held bead", "deepseek-flash", time.Minute),
	}
	dom := newPageDOM(t)
	renderWorkReading(t, dom, st)

	read := func(table string) []string {
		t.Helper()
		v, err := dom.vm.RunString(`JSON.stringify((function () {
  var t = __find(` + table + `, ".ltable") || __find(` + table + `, ".wtable");
  if (!t) return null;
  return t.children[0].children.__arr.map(function (c) { return c.textContent; });
})())`)
		if err != nil {
			t.Fatalf("reading the %s header: %v", table, err)
		}
		var heads []string
		if err := json.Unmarshal([]byte(v.String()), &heads); err != nil {
			t.Fatalf("reading the %s header: %v", table, err)
		}
		return heads
	}

	seatHead := read(`document.getElementById("polecats")`)
	if got, want := strings.Join(seatHead, "|"), "Polecat|Model|Bead|State|Age"; got != want {
		t.Errorf("the Polecats table heads %q, want %q", got, want)
	}
	landHead := read(`document.getElementById("trend")`)
	if got, want := strings.Join(landHead, "|"), "When|Polecat|Model|Bead|State|Polecat time|Score|Gate|om|Ship"; got != want {
		t.Errorf("the Landings table heads %q, want %q", got, want)
	}

	// A drawn row fills exactly the columns its table heads, so a cell added to
	// one and not the other is caught here.
	rows := oneLineRows(t, dom)
	if len(rows) == 0 {
		t.Fatal("the landings table drew no rows")
	}
	for i, r := range rows {
		if r.Cells != len(landHead) {
			t.Errorf("landings row %d drew %d cells, want the %d its header names", i, r.Cells, len(landHead))
		}
	}
}

// everyFigureReading is one landed row carrying every figure the table showed
// before it moved onto one line: a title, a commit, the stage times, a route, a
// score, the ship time, the polecat's time with its attempts and the risk flag.
func everyFigureReading(now time.Time) map[string]any {
	return map[string]any{"trend": map[string]any{
		"hours":  []map[string]any{{"hour": now.Add(-time.Hour).Format(time.RFC3339), "landed": 1, "rejected": 0}},
		"stages": []any{},
		"recent": []any{map[string]any{
			"at": now.Add(-5 * time.Minute).Format(time.RFC3339), "bead": "gt-figures", "rig": "gastown",
			"outcome": "landed", "polecat": "dag", "priority": 2, "title": "every figure kept",
			"commit": "deadbeef", "route": "overseer-heron", "risk": true,
			"score": 0.72, "lint_secs": 4.0, "gate_secs": 42.0, "om_secs": 61.0, "ship_secs": 180.0,
			"polecat_secs": 1380.0,
			"polecat_attempts": []map[string]any{
				{"seat": "gastown/opal", "secs": 840.0},
				{"seat": "gastown/jade", "secs": 540.0},
			},
		}},
	}}
}

// The one-line row drops no figure: the whole title, the commit and the stage
// times ride the Bead cell's hover, the risk flag stays beside the bead id, the
// Polecat time cell keeps its attempts on hover, and the Score, Gate, om and
// Ship cells keep their readings (gt-ultode).
func TestEveryLandingsFigureSurvivesTheOneLine(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", everyFigureReading(now))

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var tbl = __find(document.getElementById("trend"), ".ltable"), tr = tbl.children[1];
  function part(e) { return e ? {text: e.textContent, title: e.title || ""} : null; }
  return {bead: part(tr.children[3]), risk: part(__find(tr.children[3], ".warnc")),
          ptime: part(tr.children[5]), score: part(tr.children[6]), gate: part(tr.children[7]),
          om: part(tr.children[8]), ship: part(tr.children[9])};
})())`)
	if err != nil {
		t.Fatalf("reading the drawn figures: %v", err)
	}
	var got struct {
		Bead  drawn `json:"bead"`
		Risk  drawn `json:"risk"`
		Ptime drawn `json:"ptime"`
		Score drawn `json:"score"`
		Gate  drawn `json:"gate"`
		Om    drawn `json:"om"`
		Ship  drawn `json:"ship"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the drawn figures: %v", err)
	}

	for _, want := range []string{"every figure kept", "commit deadbeef", "lint 4s", "gate 42s", "om 61s", "route overseer-heron"} {
		if !strings.Contains(got.Bead.Title, want) {
			t.Errorf("the Bead cell's hover reads %q, want it to keep %q", got.Bead.Title, want)
		}
	}
	if got.Risk.Text != " ⚑" || got.Risk.Title != "touched risk paths" {
		t.Errorf("the risk flag reads %q (%q), want it beside the bead id", got.Risk.Text, got.Risk.Title)
	}
	if got.Ptime.Text != "23m" || !strings.Contains(got.Ptime.Title, "2 attempts: 14m + 9m") {
		t.Errorf("the Polecat time cell reads %q (%q), want 23m with its attempts on hover", got.Ptime.Text, got.Ptime.Title)
	}
	for _, c := range []struct {
		name string
		got  drawn
		want string
	}{
		{"Score", got.Score, "0.72"},
		{"Gate", got.Gate, "42s"},
		{"om", got.Om, "61s"},
		{"Ship", got.Ship, "3m"},
	} {
		if c.got.Text != c.want {
			t.Errorf("the %s cell reads %q, want %q", c.name, c.got.Text, c.want)
		}
	}
}

// A rejected or failing row keeps its detail, which used to be a second line:
// it reads on the State pill's hover and on the row's hover, so nothing the row
// explained is dropped when the line goes (gt-ultode).
func TestRejectedAndFailingRowsKeepTheirDetailOnHover(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 16, 0, 0, 0, time.Local)
	p := newPageDOM(t)
	p.call(t, "renderTrend", landingsOutcomes(now))

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var tbl = __find(document.getElementById("trend"), ".ltable");
  function part(e) { return e ? {text: e.textContent, title: e.title || ""} : null; }
  return tbl.children.__arr.slice(1).map(function (tr) {
    return {row: tr.title || "", pill: part(__find(tr.children[4], ".tag"))};
  });
})())`)
	if err != nil {
		t.Fatalf("reading the drawn details: %v", err)
	}
	var rows []struct {
		Row  string `json:"row"`
		Pill drawn  `json:"pill"`
	}
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the drawn details: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("the table drew %d rows, want 5", len(rows))
	}

	rejected := rows[2]
	if !strings.Contains(rejected.Pill.Text, "rejected · gate") {
		t.Errorf("the rejected row's State pill reads %q, want the kind it was refused for", rejected.Pill.Text)
	}
	if !strings.Contains(rejected.Pill.Title, "the gate failed: TestTheThing") {
		t.Errorf("the rejected row's State pill hover reads %q, want the refusal detail", rejected.Pill.Title)
	}
	if !strings.Contains(rejected.Row, "the gate failed: TestTheThing") {
		t.Errorf("the rejected row's hover reads %q, want the refusal detail", rejected.Row)
	}

	failing := rows[3]
	if !strings.Contains(failing.Pill.Text, "failing · om") {
		t.Errorf("the failing row's State pill reads %q, want the stage it failed at", failing.Pill.Text)
	}
	for _, want := range []string{"3 in a row", "the review timed out"} {
		if !strings.Contains(failing.Pill.Title, want) || !strings.Contains(failing.Row, want) {
			t.Errorf("the failing row's detail reads %q / %q, want it to keep %q on the pill and the row", failing.Pill.Title, failing.Row, want)
		}
	}
}

// The Polecat and Model tracks take their width from one set of custom
// properties the two tables declare together: a rule that sets either width
// must name both tables, and both templates must use them, so neither table can
// size its own copy of the two columns (gt-ultode).
func TestTheTwoTablesShareTheirTrackWidths(t *testing.T) {
	t.Parallel()

	shared := map[string]bool{}
	for _, rule := range strings.Split(withoutMediaBlocks(pageStyle(t)), "}") {
		sel, body, ok := strings.Cut(rule, "{")
		if !ok {
			continue
		}
		for _, prop := range []string{"--polecat-w", "--model-w"} {
			if !strings.Contains(body, prop+":") {
				continue
			}
			shared[prop] = true
			if !selectsClass(sel, "wtable") || !selectsClass(sel, "ltable") {
				t.Errorf("%s sets %s on %q, want one rule naming both .wtable and .ltable so the two tables cannot drift apart", prop, strings.TrimSpace(sel), prop)
			}
		}
	}
	for _, prop := range []string{"--polecat-w", "--model-w"} {
		if !shared[prop] {
			t.Errorf("no rule declares %s, so the two tables do not share one set of track widths", prop)
		}
	}
	for _, cls := range []string{"wtable", "ltable"} {
		cols := declOf(t, topRule(t, "."+cls), "grid-template-columns")
		for _, prop := range []string{"--polecat-w", "--model-w"} {
			if !strings.Contains(cols, "var("+prop+")") {
				t.Errorf("the .%s template reads %q, want it to take a track from var(%s)", cls, cols, prop)
			}
		}
	}
}
