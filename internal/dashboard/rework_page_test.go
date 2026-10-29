package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

// The Cost panel draws the om rework table under its Usage part. These tests
// run the page's own renderCost over a reading and read back what it drew
// (gt-mctxx).

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

func costSpendUsage() map[string]any {
	return map[string]any{
		"spend": map[string]any{"balance": 12.5, "live": map[string]any{"ok": true}},
		"usage": map[string]any{"window": "24h"},
	}
}

// The table has one row per bar with the columns the operator watches, a
// window with no reviews says so, and the unknown bar reads "?".
func TestCostPanelDrawsTheReworkTable(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = reworkPayload()
	kids, _ := drawCostChildren(t, s)

	var heads []string
	var tables [][][]string
	var subs []string
	for _, k := range kids {
		switch {
		case k.Tag == "H3" && strings.HasPrefix(k.Text, "om rework"):
			subs = append(subs, k.Text)
		case k.Tag == "TABLE" && len(k.Rows) > 0 && k.Rows[0][0] == "Bar":
			heads, tables = k.Rows[0], append(tables, k.Rows)
		}
	}
	if len(subs) != 2 || subs[0] != "om rework · last 7 days" || subs[1] != "om rework · the 7 days before" {
		t.Fatalf("rework headings = %q, want one per window", subs)
	}
	if strings.Join(heads, "|") != "Bar|Reviews|First try|Rework|Reviews per landed|Mean om time|Loops out|Mean score" {
		t.Errorf("columns = %q", heads)
	}
	if len(tables) != 1 {
		t.Fatalf("drew %d rework tables, want one: the empty window has none", len(tables))
	}
	want := [][]string{
		{"0.70", "2", "0/1", "50%", "2.00", "2:25", "0", "0.71"},
		{"0.80", "7", "1/3", "71%", "2.00", "3:10", "1", "0.58"},
		{"?", "1", "0/1", "100%", "–", "–", "0", "0.40"},
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
	for _, k := range kids {
		empty = empty || (k.Class == "empty" && k.Text == "no om reviews")
	}
	if !empty {
		t.Error("the window with no reviews does not say so")
	}
}

// The Mean om time cell's hover carries what the figure is made of: how many
// landed beads it averages, the median bead total, the slowest one and how many
// reviews had no om time to count. A bar with no such bead has nothing to say.
func TestCostPanelHoversTheMeanOMTime(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = reworkPayload()
	dom := newPageDOM(t)
	dom.call(t, "renderCost", s)
	v, err := dom.vm.RunString(`JSON.stringify(__byId["cost"].children
		.filter(c => c.tagName === "TABLE" && c.children[0].children.map(x => x.textContent).indexOf("Mean om time") >= 0)
		.map(t => t.children.slice(1).map(r => r.children[5].title)))`)
	if err != nil {
		t.Fatalf("reading the column's hovers: %v", err)
	}
	var tables [][]string
	if err := json.Unmarshal([]byte(v.String()), &tables); err != nil {
		t.Fatalf("reading the column's hovers: %v", err)
	}
	if len(tables) != 1 {
		t.Fatalf("read %d tables, want the one rework table", len(tables))
	}
	want := []string{
		"mean of 1 landed bead, each the sum of its reviews · median 2:25 · slowest gt-a 2:25 · 1 review with no om time",
		"mean of 2 landed beads, each the sum of its reviews · median 3:10 · slowest gt-c 5:00",
		"",
	}
	if len(tables[0]) != len(want) {
		t.Fatalf("hovers = %q, want one per row", tables[0])
	}
	for i := range want {
		if tables[0][i] != want[i] {
			t.Errorf("row %d hover = %q, want %q", i, tables[0][i], want[i])
		}
	}
}

// An unreadable daemon log is named above the table, which still draws: the
// review figures are the review log's own and the missing part is the om time.
func TestCostPanelNamesAnUnreadableOMTimeLog(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	r := reworkPayload()
	r["om_unavailable"] = true
	s["rework"] = r
	kids, _ := drawCostChildren(t, s)
	var named, tables int
	for _, k := range kids {
		if k.Class == "badc" && strings.Contains(k.Text, "daemon log could not be read") {
			named++
		}
		if k.Tag == "TABLE" {
			tables++
		}
	}
	if named != 1 || tables == 0 {
		t.Errorf("drew %d naming lines and %d tables, want one line and the table", named, tables)
	}
}

// A read that failed is named, and a read still pending is reading; neither
// draws a table, and the panel's section contract stays.
func TestCostPanelNamesTheReworkStates(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = map[string]any{"unavailable": true}
	kids, _ := drawCostChildren(t, s)
	last := kids[len(kids)-1]
	if last.Class != "badc" || !strings.Contains(last.Text, "review log unreadable") {
		t.Errorf("unreadable log drew %+v, want a badc line naming it", last)
	}

	kids, note := drawCostChildren(t, costSpendUsage())
	last = kids[len(kids)-1]
	if last.Class != "empty" || !strings.Contains(last.Text, "om rework") || !strings.Contains(last.Text, "reading") {
		t.Errorf("pending read drew %+v, want a reading line", last)
	}
	if strings.Contains(note.Text, "undefined") {
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
	dom := newPageDOM(t)
	dom.call(t, "renderCost", s)
	v, err := dom.vm.RunString(`JSON.stringify(__byId["cost"].children
		.filter(c => c.tagName === "TABLE" && c.children[0].children.map(x => x.textContent).indexOf("Mean om time") >= 0)
		.map(t => t.children.slice(1).map(r => [r.children[5].textContent, r.children[5].title])))`)
	if err != nil {
		t.Fatalf("reading the column: %v", err)
	}
	var tables [][][]string
	if err := json.Unmarshal([]byte(v.String()), &tables); err != nil {
		t.Fatalf("reading the column: %v", err)
	}
	if len(tables) != 1 || len(tables[0]) != 1 {
		t.Fatalf("read %v, want the one row of the one rework table", tables)
	}
	if got := tables[0][0]; got[0] != "–" || got[1] != "3 reviews with no om time" {
		t.Errorf("cell = %q, want a dash hovering %q", got, "3 reviews with no om time")
	}
}
