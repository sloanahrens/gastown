package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The Cost panel draws the om rework table in a fold of its own, closed like its
// siblings, under its Usage part. These tests run the page's own renderCost over
// a reading and read back what it drew (gt-mctxx, gt-uapj6).

// reworkChild is one top-level child of the Cost panel, with the cell text of
// a table.
type reworkChild struct {
	Tag   string     `json:"tag"`
	Class string     `json:"class"`
	Text  string     `json:"text"`
	Rows  [][]string `json:"rows"`
}

func drawCostChildren(t *testing.T, s map[string]any) (kids []reworkChild, note drawn) {
	t.Helper()
	dom := newPageDOM(t)
	dom.call(t, "renderCost", s)
	v, err := dom.vm.RunString(`JSON.stringify(__byId["cost"].children.map(c => ({tag: c.tagName, class: c.className, text: c.textContent,
		rows: c.tagName === "TABLE" ? c.children.map(r => r.children.map(x => x.textContent)) : null})))`)
	if err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	if err := json.Unmarshal([]byte(v.String()), &kids); err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	return kids, dom.element(t, "costnote")
}

// drawRework runs renderCost over s and reads back the om rework fold, failing
// when the page keeps none or titles it something else.
func drawRework(t *testing.T, s map[string]any) (*pageDOM, costFoldState) {
	t.Helper()
	dom := newPageDOM(t)
	dom.call(t, "renderCost", s)
	state := readCostFold(t, dom, "cost.rework")
	v, err := dom.vm.RunString(`String(costFolds["cost.rework"].head._text || "")`)
	if err != nil {
		t.Fatalf("reading the rework fold's title: %v", err)
	}
	if got := v.String(); got != "om rework" {
		t.Fatalf("the fold is titled %q, want om rework", got)
	}
	return dom, state
}

func reworkPayload() map[string]any {
	return map[string]any{"windows": []any{
		map[string]any{"name": "last 7 days", "bars": []any{
			map[string]any{"bar": 0.7, "reviews": 2, "rejected": 1, "beads": 1, "first_try": 0, "landed": 1, "reviews_per_landed": 2.0, "loops_out": 0, "mean_score": 0.71,
				"mean_om_secs": 145.0, "om_beads": 1, "median_om_secs": 145.0, "slowest_om_secs": 145.0, "slowest_om_bead": "gt-a", "om_reviews_missing": 1},
			map[string]any{"bar": 0.8, "reviews": 7, "rejected": 5, "beads": 3, "first_try": 1, "landed": 2, "reviews_per_landed": 2.0, "loops_out": 1, "mean_score": 0.5786,
				"mean_om_secs": 190.0, "om_beads": 2, "median_om_secs": 190.0, "slowest_om_secs": 300.0, "slowest_om_bead": "gt-c"},
			map[string]any{"bar": 0, "reviews": 1, "rejected": 1, "beads": 1, "first_try": 0, "landed": 0, "reviews_per_landed": 0, "loops_out": 0, "mean_score": 0.4},
		}},
		map[string]any{"name": "the 7 days before", "bars": []any{}},
	}}
}

// reworkWindowReading is one window of the table with the span the reader sends,
// so the page can date it itself: the window that ends now and the two before it.
func reworkWindowReading(now time.Time, from, to time.Duration, reviews int) map[string]any {
	return map[string]any{
		"name": "the 7 days before",
		"from": now.Add(-from).UTC().Format(time.RFC3339),
		"to":   now.Add(-to).UTC().Format(time.RFC3339),
		"bars": []any{reworkBarReading(reviews)},
	}
}

// reworkBarReading is one clean bar of the table with the given reviews.
func reworkBarReading(reviews int) map[string]any {
	return map[string]any{"bar": 0.8, "reviews": reviews, "rejected": 0, "beads": 1, "first_try": 1, "landed": 1, "reviews_per_landed": 1.0, "loops_out": 0, "mean_score": 0.8}
}

func costSpendUsage() map[string]any {
	return map[string]any{
		"spend": map[string]any{"balance": 12.5, "live": map[string]any{"ok": true}},
		"usage": map[string]any{"window": "24h"},
	}
}

// The fold is titled om rework, closed on load like its siblings, and holds one
// table over both windows with a Window column in front and a row per bar. A
// window with no reviews says so under the table rather than drawing an empty
// table of its own.
func TestCostPanelDrawsTheReworkTable(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = reworkPayload()
	dom, fold := drawRework(t, s)
	if !fold.Closed {
		t.Error("the om rework fold is open on load, want it closed like its siblings")
	}

	tables := costFoldTables(t, dom, "cost.rework")
	if len(tables) != 1 {
		t.Fatalf("drew %d rework tables, want one for both windows", len(tables))
	}
	if heads := strings.Join(tables[0][0], "|"); heads != "Window|Bar|Reviews|First try|Rework|Reviews per landed|Mean om time|Loops out|Mean score" {
		t.Errorf("columns = %q", heads)
	}
	want := [][]string{
		{"last 7 days", "0.70", "2", "0/1", "50%", "2.00", "2:25", "0", "0.71"},
		{"last 7 days", "0.80", "7", "1/3", "71%", "2.00", "3:10", "1", "0.58"},
	}
	rows := tables[0][1:]
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want %v", rows, want)
	}
	for i := range want {
		if strings.Join(rows[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("row %d = %v, want %v", i, rows[i], want[i])
		}
	}
	empty := false
	for _, line := range costFoldLines(t, dom, "cost.rework") {
		empty = empty || (line.Class == "empty" && line.Text == "the 7 days before: no om reviews")
	}
	if !empty {
		t.Error("the window with no reviews does not say so")
	}
}

// A bar of 0 is a tree that named none, so the row reads "?" and keeps its
// reviews.
func TestCostPanelDrawsTheUnknownBar(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = map[string]any{"windows": []any{
		map[string]any{"name": "last 7 days", "bars": []any{
			map[string]any{"bar": 0, "reviews": 2, "rejected": 0, "beads": 1, "first_try": 1, "landed": 1, "reviews_per_landed": 2.0, "loops_out": 0, "mean_score": 0.8},
		}},
	}}
	dom, _ := drawRework(t, s)

	tables := costFoldTables(t, dom, "cost.rework")
	if len(tables) != 1 || len(tables[0]) != 2 {
		t.Fatalf("drew %v, want the one row of the one rework table", tables)
	}
	if row := tables[0][1]; row[1] != "?" || row[2] != "2" {
		t.Errorf("row = %v, want a ? bar with its 2 reviews", row)
	}
}

// The closed heading says what rework cost: the latest window's share, its loops
// out and the window it reached.
func TestCostPanelHeadlineNamesWeeksReworkAndLoops(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = reworkPayload()
	_, fold := drawRework(t, s)
	if want := "last 7 days: 70% rework · 1 loop out"; fold.Summary != want {
		t.Errorf("closed line = %q, want %q", fold.Summary, want)
	}

	// A week with no loop keeps the plural out of the line, and a week with no
	// reviews says that instead of dividing by zero.
	s["rework"] = map[string]any{"windows": []any{
		map[string]any{"name": "last 7 days", "bars": []any{
			map[string]any{"bar": 0.8, "reviews": 4, "rejected": 2, "beads": 2, "first_try": 1, "landed": 2, "reviews_per_landed": 2.0, "loops_out": 0, "mean_score": 0.8},
		}},
	}}
	_, fold = drawRework(t, s)
	if want := "last 7 days: 50% rework · 0 loops out"; fold.Summary != want {
		t.Errorf("closed line = %q, want %q", fold.Summary, want)
	}

	s["rework"] = map[string]any{"windows": []any{map[string]any{"name": "last 7 days"}}}
	_, fold = drawRework(t, s)
	if want := "no om reviews, last 7 days"; fold.Summary != want {
		t.Errorf("closed line = %q, want %q", fold.Summary, want)
	}
}

// The older stretches are dated by their own span, so the seven days before the
// last seven cannot read the same as the week before that.
func TestCostPanelDatesTheOlderReworkWindows(t *testing.T) {
	t.Parallel()

	now := time.Now()
	day := 24 * time.Hour
	s := costSpendUsage()
	s["rework"] = map[string]any{"windows": []any{
		reworkWindowReading(now, 7*day, 0, 9),
		reworkWindowReading(now, 14*day, 7*day, 6),
		reworkWindowReading(now, 21*day, 14*day, 4),
	}}
	dom, _ := drawRework(t, s)

	tables := costFoldTables(t, dom, "cost.rework")
	if len(tables) != 1 {
		t.Fatalf("drew %d rework tables, want one", len(tables))
	}
	var got []string
	for _, row := range tables[0][1:] {
		got = append(got, row[0])
	}
	want := []string{"last 7 days", "7-14 days ago", "14-21 days ago"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("window column = %q, want %q", got, want)
	}
}

// A window that ends now is named for the span it covers, so a reader's own
// "the 7 days before" does not land on the week that just closed.
func TestCostPanelNamesTheWindowEndingNow(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := costSpendUsage()
	s["rework"] = map[string]any{"windows": []any{reworkWindowReading(now, 2*24*time.Hour, 0, 3)}}
	dom, _ := drawRework(t, s)

	tables := costFoldTables(t, dom, "cost.rework")
	if len(tables) != 1 || len(tables[0]) != 2 {
		t.Fatalf("drew %v, want the one row of the one rework table", tables)
	}
	if got := tables[0][1][0]; got != "last 2 days" {
		t.Errorf("window column = %q, want last 2 days", got)
	}
}

// A window whose span is missing or unreadable keeps the name the reader sent,
// since a dated label the page cannot compute would be a guess.
func TestCostPanelNamesAnUndatedWindowByItsReaderName(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = map[string]any{"windows": []any{
		map[string]any{"name": "no span sent", "bars": []any{reworkBarReading(3)}},
		map[string]any{"name": "span unreadable", "from": "yesterday", "to": "today", "bars": []any{reworkBarReading(3)}},
	}}
	dom, _ := drawRework(t, s)

	tables := costFoldTables(t, dom, "cost.rework")
	if len(tables) != 1 {
		t.Fatalf("drew %d rework tables, want one", len(tables))
	}
	var got []string
	for _, row := range tables[0][1:] {
		got = append(got, row[0])
	}
	if want := []string{"no span sent", "span unreadable"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("window column = %q, want %q", got, want)
	}
}

// A window's bar that saw a single review draws no row: one review is 1/1 and
// 0% beside bars that carry enough reviews to say something. A window left with
// no row at all draws nothing and does not claim it had no reviews.
func TestCostPanelDropsSingleReviewRows(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = map[string]any{"windows": []any{
		map[string]any{"name": "last 7 days", "bars": []any{
			map[string]any{"bar": 0.8, "reviews": 1, "rejected": 0, "beads": 1, "first_try": 1, "landed": 1, "reviews_per_landed": 1.0, "loops_out": 0, "mean_score": 0.8},
			map[string]any{"bar": 0.7, "reviews": 3, "rejected": 1, "beads": 2, "first_try": 1, "landed": 2, "reviews_per_landed": 1.5, "loops_out": 0, "mean_score": 0.7},
		}},
		map[string]any{"name": "the 7 days before", "bars": []any{
			map[string]any{"bar": 0.6, "reviews": 1, "rejected": 0, "beads": 1, "first_try": 1, "landed": 1, "reviews_per_landed": 1.0, "loops_out": 0, "mean_score": 0.6},
		}},
	}}
	dom, _ := drawRework(t, s)

	tables := costFoldTables(t, dom, "cost.rework")
	if len(tables) != 1 {
		t.Fatalf("drew %d rework tables, want one", len(tables))
	}
	rows := tables[0][1:]
	if len(rows) != 1 || rows[0][1] != "0.70" || rows[0][2] != "3" {
		t.Errorf("rows = %v, want only the bar that reviewed more than once", rows)
	}
	for _, line := range costFoldLines(t, dom, "cost.rework") {
		if strings.HasSuffix(line.Text, ": no om reviews") {
			t.Errorf("drew %q for a window whose only bar reviewed once", line.Text)
		}
	}
}

// The Mean om time cell's hover carries what the figure is made of: how many
// landed beads it averages, the median bead total, the slowest one and how many
// reviews had no om time to count. A bar with no such bead has nothing to say.
func TestCostPanelHoversTheMeanOMTime(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = reworkPayload()
	dom, _ := drawRework(t, s)
	hovers := omTimeHovers(t, dom)
	want := []string{
		"mean of 1 landed bead, each the sum of its reviews · median 2:25 · slowest gt-a 2:25 · 1 review with no om time",
		"mean of 2 landed beads, each the sum of its reviews · median 3:10 · slowest gt-c 5:00",
	}
	if len(hovers) != len(want) {
		t.Fatalf("hovers = %q, want one per row", hovers)
	}
	for i := range want {
		if hovers[i] != want[i] {
			t.Errorf("row %d hover = %q, want %q", i, hovers[i], want[i])
		}
	}
}

// omTimeHovers reads the Mean om time cell of every row of the rework table,
// text and hover together.
func omTimeHovers(t *testing.T, dom *pageDOM) []string {
	t.Helper()
	v, err := dom.vm.RunString(`JSON.stringify(costFolds["cost.rework"].body.children
		.filter(c => c.tagName === "TABLE")
		.reduce((all, tb) => all.concat(tb.children.slice(1).map(r => r.children[6].title)), []))`)
	if err != nil {
		t.Fatalf("reading the column's hovers: %v", err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the column's hovers: %v", err)
	}
	return out
}

// An unreadable daemon log is named in the fold, which still draws the table:
// the review figures are the review log's own and the missing part is the om
// time.
func TestCostPanelNamesAnUnreadableOMTimeLog(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	r := reworkPayload()
	r["om_unavailable"] = true
	s["rework"] = r
	dom, _ := drawRework(t, s)

	var named int
	for _, line := range costFoldLines(t, dom, "cost.rework") {
		if line.Class == "badc" && strings.Contains(line.Text, "daemon log could not be read") {
			named++
		}
	}
	if tables := costFoldTables(t, dom, "cost.rework"); named != 1 || len(tables) == 0 {
		t.Errorf("drew %d naming lines and %d tables, want one line and the table", named, len(tables))
	}
}

// A read that failed is named, and a read still pending is reading; neither
// draws a table, and the panel's section contract stays.
func TestCostPanelNamesTheReworkStates(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = map[string]any{"unavailable": true}
	dom, fold := drawRework(t, s)
	lines := costFoldLines(t, dom, "cost.rework")
	if len(lines) != 1 || lines[0].Class != "badc" || !strings.Contains(lines[0].Text, "review log unreadable") {
		t.Errorf("unreadable log drew %+v, want one badc line naming it", lines)
	}
	if fold.Summary != "review log unreadable" {
		t.Errorf("unreadable log's closed line = %q, want it named there too", fold.Summary)
	}

	dom, fold = drawRework(t, costSpendUsage())
	lines = costFoldLines(t, dom, "cost.rework")
	if len(lines) != 1 || lines[0].Class != "empty" || !strings.Contains(lines[0].Text, "reading") {
		t.Errorf("pending read drew %+v, want one reading line", lines)
	}
	if fold.Summary != "reading…" {
		t.Errorf("pending read's closed line = %q, want reading", fold.Summary)
	}
	if _, note := drawCostChildren(t, costSpendUsage()); strings.Contains(note.Text, "undefined") {
		t.Errorf("caption = %q", note.Text)
	}

	page := string(indexHTML)
	if !strings.Contains(page, `<section data-fold="cost"><h2>Cost <span class="r" id="costnote"></span></h2><div class="body" id="cost"></div></section>`) {
		t.Error("the Cost section is no longer a section whose h2 holds the title and a span.r")
	}
	cost := pageFunc(t, "renderCost")
	if usage, rework := strings.Index(cost, "costUsage("), strings.Index(cost, "costRework(box, s.rework)"); rework < usage || rework < 0 {
		t.Error("renderCost does not draw the rework table after the Usage part")
	}
	body := pageFunc(t, "costRework")
	for _, bad := range []string{"innerHTML", "insertAdjacentHTML", "outerHTML", "document.write"} {
		if strings.Contains(body, bad) {
			t.Errorf("costRework uses %s", bad)
		}
	}
}

// A bar that landed beads but has no om time for any of them draws a dash, and
// its hover names only the reviews that had none: no mean, median or slowest
// bead to describe.
func TestCostPanelHoversABarWithNoLandedOMTime(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = map[string]any{"windows": []any{
		map[string]any{"name": "last 7 days", "bars": []any{
			map[string]any{"bar": 0.8, "reviews": 3, "rejected": 1, "beads": 2, "first_try": 1, "landed": 2, "reviews_per_landed": 1.5, "loops_out": 0, "mean_score": 0.8,
				"mean_om_secs": 0.0, "om_beads": 0, "median_om_secs": 0.0, "slowest_om_secs": 0.0, "om_reviews_missing": 3},
		}},
	}}
	dom, _ := drawRework(t, s)
	v, err := dom.vm.RunString(`JSON.stringify(costFolds["cost.rework"].body.children
		.filter(c => c.tagName === "TABLE")
		.reduce((all, tb) => all.concat(tb.children.slice(1).map(r => [r.children[6].textContent, r.children[6].title])), []))`)
	if err != nil {
		t.Fatalf("reading the column: %v", err)
	}
	var cells [][]string
	if err := json.Unmarshal([]byte(v.String()), &cells); err != nil {
		t.Fatalf("reading the column: %v", err)
	}
	if len(cells) != 1 {
		t.Fatalf("read %v, want the one row of the one rework table", cells)
	}
	if got := cells[0]; got[0] != "–" || got[1] != "3 reviews with no om time" {
		t.Errorf("cell = %q, want a dash hovering %q", got, "3 reviews with no om time")
	}
}
