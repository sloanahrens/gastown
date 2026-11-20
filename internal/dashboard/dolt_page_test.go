package dashboard

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// doltCell is one table cell the Dolt panel drew: the text an operator reads
// and how many elements sit under it, so a cell holding markup reads as the
// markup rather than as a drawn element.
type doltCell struct {
	Text string `json:"text"`
	Kids int    `json:"kids"`
}

// doltPart is one child of the Dolt panel's body: a section's heading, its
// table (its head cells and rows in the order drawn), or the one line a
// section with no table draws.
type doltPart struct {
	Tag   string       `json:"tag"`
	Class string       `json:"class"`
	Text  string       `json:"text"`
	Head  []string     `json:"head"`
	Rows  [][]doltCell `json:"rows"`
}

// doltPartsJS reads every child of the Dolt panel's body back as a doltPart,
// descending into a table's head and body so a test compares the cells the
// page drew rather than the container they went into.
const doltPartsJS = `JSON.stringify((function () {
  var box = __byId["dolt"], out = [];
  for (var i = 0; i < box.children.length; i++) {
    var c = box.children[i];
    var d = {tag: c.tagName, class: c.className, text: c.textContent, head: [], rows: []};
    if (c.tagName === "TABLE") {
      for (var j = 0; j < c.children.length; j++) {
        var part = c.children[j];
        if (part.tagName === "THEAD") {
          var hr = part.children[0];
          for (var k = 0; k < hr.children.length; k++) d.head.push(hr.children[k].textContent);
        } else if (part.tagName === "TBODY") {
          for (var k = 0; k < part.children.length; k++) {
            var tr = part.children[k], row = [];
            for (var m = 0; m < tr.children.length; m++) row.push({text: tr.children[m].textContent, kids: tr.children[m].children.length});
            d.rows.push(row);
          }
        }
      }
    }
    out.push(d);
  }
  return out;
})())`

// drawDolt runs renderDolt over s and reads back the panel's body, its heading
// note and the section itself.
func drawDolt(t *testing.T, s map[string]any) (parts []doltPart, note, sec drawn) {
	t.Helper()

	dom := newPageDOM(t)
	dom.call(t, "renderDolt", s)
	v, err := dom.vm.RunString(doltPartsJS)
	if err != nil {
		t.Fatalf("reading the Dolt panel: %v", err)
	}
	if err := json.Unmarshal([]byte(v.String()), &parts); err != nil {
		t.Fatalf("reading the Dolt panel: %v", err)
	}
	return parts, dom.element(t, "doltnote"), dom.element(t, "doltsec")
}

// A panel with two sections draws a heading for each — its title and note — and
// under it a table of the section's head and rows in the order the hub read
// them.
func TestDoltPageDrawsATablePerSection(t *testing.T) {
	t.Parallel()

	parts, note, sec := drawDolt(t, map[string]any{"dolt": map[string]any{
		"sections": []any{
			map[string]any{
				"key": "latency", "title": "Query latency", "note": "the last five minutes",
				"head": []any{"p50", "p95"},
				"rows": []any{[]any{"1ms", "2ms"}, []any{"3ms", "9ms"}},
			},
			map[string]any{
				"key": "sessions", "title": "Open sessions", "note": "who holds the lock",
				"head": []any{"from"},
				"rows": []any{[]any{"sandbox"}},
			},
		},
	}})

	if sec.Hidden {
		t.Error("the Dolt panel is hidden while the hub carries a panel")
	}
	if note.Text != "2 sections" {
		t.Errorf("the heading note reads %q, want %q", note.Text, "2 sections")
	}
	if len(parts) != 4 {
		t.Fatalf("the panel body holds %d children, want two headings and their tables: %+v", len(parts), parts)
	}
	for _, want := range []string{"Query latency", "the last five minutes"} {
		if !strings.Contains(parts[0].Text, want) {
			t.Errorf("the first heading reads %q, want it to name %q", parts[0].Text, want)
		}
	}
	if got, want := parts[1].Head, []string{"p50", "p95"}; !slices.Equal(got, want) {
		t.Errorf("the first table heads %v, want %v", got, want)
	}
	wantRows := [][]string{{"1ms", "2ms"}, {"3ms", "9ms"}}
	if len(parts[1].Rows) != len(wantRows) {
		t.Fatalf("the first table drew %d rows, want %d: %+v", len(parts[1].Rows), len(wantRows), parts[1].Rows)
	}
	for i, want := range wantRows {
		for j, cell := range want {
			if got := parts[1].Rows[i][j].Text; got != cell {
				t.Errorf("the first table's row %d cell %d reads %q, want %q", i, j, got, cell)
			}
		}
	}
	for _, want := range []string{"Open sessions", "who holds the lock"} {
		if !strings.Contains(parts[2].Text, want) {
			t.Errorf("the second heading reads %q, want it to name %q", parts[2].Text, want)
		}
	}
	if got, want := parts[3].Head, []string{"from"}; !slices.Equal(got, want) {
		t.Errorf("the second table heads %v, want %v", got, want)
	}
	if len(parts[3].Rows) != 1 || parts[3].Rows[0][0].Text != "sandbox" {
		t.Errorf("the second table drew %+v, want one row holding %q", parts[3].Rows, "sandbox")
	}
}

// A section whose read failed draws one line naming the failure, in the warning
// colour, where its table would be, and the heading note counts it: the page
// never draws an empty table over a failure, which would read as a section the
// window emptied.
func TestDoltPageDrawsAFailedSectionAsOneLine(t *testing.T) {
	t.Parallel()

	parts, note, _ := drawDolt(t, map[string]any{"dolt": map[string]any{
		"sections": []any{
			map[string]any{"key": "latency", "title": "Query latency", "head": []any{"p50"}, "rows": []any{[]any{"1ms"}}},
			map[string]any{"key": "sessions", "title": "Open sessions", "err": "dial tcp: connection refused"},
		},
	}})

	if note.Text != "2 sections, 1 unreadable" {
		t.Errorf("the heading note reads %q, want %q", note.Text, "2 sections, 1 unreadable")
	}
	if note.Class != "r warnc" {
		t.Errorf("a note counting an unreadable section wears %q, want the warning colour", note.Class)
	}
	tables := 0
	for _, p := range parts {
		if p.Tag == "TABLE" {
			tables++
		}
	}
	if tables != 1 {
		t.Fatalf("the panel drew %d tables, want only the readable section's one: %+v", tables, parts)
	}
	if len(parts) != 4 {
		t.Fatalf("the panel body holds %d children, want two headings, one table and one line: %+v", len(parts), parts)
	}
	if !strings.Contains(parts[2].Text, "Open sessions") {
		t.Errorf("the failed section's heading reads %q, want it to name %q", parts[2].Text, "Open sessions")
	}
	if parts[3].Tag != "DIV" {
		t.Fatalf("the failed section drew a %s, want one line", parts[3].Tag)
	}
	if want := "unreadable: dial tcp: connection refused"; parts[3].Text != want {
		t.Errorf("the failed section draws %q, want %q", parts[3].Text, want)
	}
	if parts[3].Class != "empty warnc" {
		t.Errorf("the failed section's line wears %q, want the warning colour", parts[3].Class)
	}
}

// A cell whose value holds markup draws the markup as text, and a section the
// window emptied of rows says so in one line rather than drawing an empty table.
func TestDoltPageDrawsCellsAsTextAndAnEmptySectionAsALine(t *testing.T) {
	t.Parallel()

	const markup = "<b>slow</b> & <script>x</script>"
	parts, note, _ := drawDolt(t, map[string]any{"dolt": map[string]any{
		"sections": []any{
			map[string]any{"key": "labels", "title": "Labels", "head": []any{"label"}, "rows": []any{[]any{markup}}},
			map[string]any{"key": "waiters", "title": "Waiters", "note": "the lock queue", "head": []any{"id"}},
		},
	}})

	if note.Text != "2 sections" {
		t.Errorf("the heading note reads %q, want %q", note.Text, "2 sections")
	}
	if got := parts[1].Rows[0][0].Text; got != markup {
		t.Errorf("the cell reads %q, want the value as written, %q", got, markup)
	}
	if got := parts[1].Rows[0][0].Kids; got != 0 {
		t.Errorf("the cell holds %d elements, want the markup drawn as text", got)
	}
	if len(parts) != 4 {
		t.Fatalf("the panel body holds %d children, want two headings, one table and one line: %+v", len(parts), parts)
	}
	if !strings.Contains(parts[2].Text, "the lock queue") {
		t.Errorf("the empty section's heading reads %q, want it to name its note", parts[2].Text)
	}
	if parts[3].Tag != "DIV" || parts[3].Text != "nothing in the window" {
		t.Errorf("the empty section drew %s %q, want the one line %q", parts[3].Tag, parts[3].Text, "nothing in the window")
	}
	if parts[3].Class != "empty" {
		t.Errorf("the empty section's line wears %q, want the page's quiet class", parts[3].Class)
	}
}

// The Dolt panel says which of its states it is in, and none of them is a blank:
// a hub with no reader and no read leaves the section out, a read that failed
// with no panel says so in red, and a panel the hub kept says above its sections
// whether the read behind it failed or went stale rather than passing for
// current (gt-055sfu).
func TestDoltPageSaysWhenTheReadFailed(t *testing.T) {
	t.Parallel()

	panel := func() map[string]any {
		return map[string]any{"sections": []any{
			map[string]any{"key": "latency", "title": "Query latency", "head": []any{"p50"}, "rows": []any{[]any{"1ms"}}},
		}}
	}
	kept := func(failed bool) map[string]any {
		return map[string]any{
			"dolt":  panel(),
			"reads": map[string]any{"dolt": map[string]any{"at": "2026-11-20T14:00:00Z", "every_sec": 300, "error": failed, "stale": true}},
		}
	}
	cases := []struct {
		name            string
		state           map[string]any
		hidden          bool
		noteText        string
		noteClass       string
		line, lineClass string
	}{
		{
			name:      "no reader and no read",
			state:     map[string]any{},
			hidden:    true,
			noteText:  "",
			noteClass: "r",
		},
		{
			name:      "a failed read with no panel",
			state:     map[string]any{"reads": map[string]any{"dolt": map[string]any{"at": "2026-11-20T14:00:00Z", "every_sec": 300, "error": true, "stale": true}}},
			noteText:  "unavailable — the read failed",
			noteClass: "r badc",
		},
		{
			name:      "a failed read that kept the last panel",
			state:     kept(true),
			noteText:  "1 section",
			noteClass: "r",
			line:      "the read failed; these are the last sections",
			lineClass: "empty badc",
		},
		{
			name:      "an old read that kept the last panel",
			state:     kept(false),
			noteText:  "1 section",
			noteClass: "r",
			line:      "these sections are stale",
			lineClass: "empty warnc",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			parts, note, sec := drawDolt(t, c.state)
			if sec.Hidden != c.hidden {
				t.Errorf("the section is hidden=%v, want %v", sec.Hidden, c.hidden)
			}
			if note.Text != c.noteText {
				t.Errorf("the heading note reads %q, want %q", note.Text, c.noteText)
			}
			if note.Class != c.noteClass {
				t.Errorf("the heading note wears %q, want %q", note.Class, c.noteClass)
			}
			if c.line == "" {
				return
			}
			if len(parts) == 0 {
				t.Fatalf("the panel drew no line saying %q", c.line)
			}
			if parts[0].Text != c.line || parts[0].Class != c.lineClass {
				t.Errorf("the panel opens with %s %q wearing %q, want %q wearing %q", parts[0].Tag, parts[0].Text, parts[0].Class, c.line, c.lineClass)
			}
			if len(parts) != 3 {
				t.Errorf("the panel body holds %d children, want the read's line and the kept section's heading and table: %+v", len(parts), parts)
			}
		})
	}
}

// The Dolt panel sits in the right column directly after the JEV panel, folds
// until the operator opens it, and is drawn on every render from the state's
// dolt key with its read's own status in the draw's read list, so a read that
// fails while its panel holds still redraws the pane.
func TestDoltPanelSitsAfterJevAndIsDrawnOnEveryPush(t *testing.T) {
	t.Parallel()

	const tag = `<section id="doltsec" data-fold="dolt" data-default-collapsed hidden><h2>Dolt · <span class="r" id="doltnote"></span></h2><div class="body" id="dolt"></div></section>`
	page := string(indexHTML)
	at := strings.Index(page, tag)
	if at < 0 {
		t.Fatalf("index.html has no Dolt panel under the markup %q, so the pane that folds and hides is not the page's", tag)
	}
	jev := strings.Index(page, `id="jevsec"`)
	if jev < 0 || jev > at {
		t.Error("the Dolt panel is not after the JEV panel")
	}
	cols := strings.Split(page, `<div class="col">`)
	if len(cols) != 3 || !strings.Contains(cols[2], `id="doltsec"`) {
		t.Error("the Dolt panel is not in the right-hand column")
	}

	dom := newPageDOM(t)
	if !dom.fold(t, "dolt").Collapsed {
		t.Error("the Dolt pane starts open on first load")
	}
	dom.press(t, "dolt", "click")
	if dom.fold(t, "dolt").Collapsed {
		t.Error("the Dolt pane does not open when its heading is clicked")
	}

	if got := pageExpr(t, dom, `PANES.dolt.sec === "dolt" && PANES.dolt.body.join(",") === "dolt" && typeof PANES.dolt.draw === "function"`); got != "true" {
		t.Error("PANES has no dolt pane drawing #dolt from the dolt section")
	}
	if got := pageExpr(t, dom, `JSON.stringify(PANES.dolt.read({dolt: 1, reads: {dolt: {at: "x", every_sec: 300, error: true, stale: true}}}))`); got != `[1,["x",300,true,true]]` {
		t.Errorf("the dolt pane's read list is %s, want the panel and its read's own status, so a read that fails while its panel holds still redraws the pane", got)
	}
	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderDolt(state)") {
		t.Error("renderAll does not draw the Dolt panel")
	}
	if n := strings.Count(page, "renderDolt(state)"); n < 2 {
		t.Errorf("the page draws renderDolt(state) %d times, want renderAll and the 15 s tick", n)
	}
}
