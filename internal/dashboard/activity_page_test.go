package dashboard

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// The Town activity pane is the town's own feed (gt-l98ykr): the hub sends the
// ring as one backlog frame on connect and each new journal line as an entry
// frame, and the page draws them. These tests run index.html's own script over
// the stream stub (page_harness_test.go) and drive the frames the server would
// send, so the pane is read back from the page rather than from its source.

// activityRow is one row of #activity as the page drew it: the row's classes and
// the text of each cell, in the order time, rig, kind, text.
type activityRow struct {
	Cls   string   `json:"cls"`
	Cells []string `json:"cells"`
}

// activityRows reads the feed's rows, newest first as the pane draws them. The
// empty feed's own line is not a row.
func activityRows(t *testing.T, dom *pageDOM) []activityRow {
	t.Helper()

	js := `JSON.stringify(__byId["activity"].children.__arr.filter(r => r.className.indexOf("atrow") === 0).map(r => ({
		cls: r.className,
		cells: r.children.__arr.map(c => c.textContent)})))`
	var out []activityRow
	if err := json.Unmarshal([]byte(pageExpr(t, dom, js)), &out); err != nil {
		t.Fatalf("reading the activity rows: %v", err)
	}
	return out
}

// activityEmptyText is what the feed says in place of rows: its line, or "" when
// it drew none.
func activityEmptyText(t *testing.T, dom *pageDOM) string {
	t.Helper()

	return pageExpr(t, dom, `__byId["activity"].children.__arr.filter(r => r.className === "empty").map(r => r.textContent).join("")`)
}

// activityHeaderNote is the pane header's note, which a shut pane shows as its
// summary line.
func activityHeaderNote(t *testing.T, dom *pageDOM) string {
	t.Helper()

	return dom.element(t, "activitynote").Text
}

// activitySummary is the line a shut pane shows in the note's place.
func activitySummary(t *testing.T, dom *pageDOM) string {
	t.Helper()

	return pageExpr(t, dom, `__foldRead("activity").summary`)
}

// activityEntry is one line the hub would send in a frame: an Entry of the shape
// internal/dashboard/types.go encodes.
func activityEntry(seq int, at time.Time, rig, kind, text, class string) map[string]any {
	return map[string]any{
		"seq":   seq,
		"at":    at.Format(time.RFC3339),
		"rig":   rig,
		"kind":  kind,
		"text":  text,
		"class": class,
	}
}

// streamActivity sends one line on the live stream, stamped on the page's own
// clock: a test that reads an exact age needs a stamp the page's clock measured,
// not a wall clock the test cannot hold still.
func streamActivity(t *testing.T, dom *pageDOM, seq int, ago time.Duration, rig, kind, text, class string) {
	t.Helper()

	js := fmt.Sprintf(`__fire(__sources[%s], "entry", {seq: %d, at: new Date(Date.now() - %d).toISOString(), rig: %s, kind: %s, text: %s, class: %s})`,
		liveStream, seq, ago.Milliseconds(), quoteJS(rig), quoteJS(kind), quoteJS(text), quoteJS(class))
	pageExpr(t, dom, js)
}

// The window's lines: three entry frames draw three rows, newest at the top,
// each reading the line's time, rig, kind and text, and wearing the class its
// colour is taken from.
func TestActivityEntryFramesDrawRowsNewestFirst(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	now := pageNow(t, dom)
	lines := []struct{ rig, kind, text, class string }{
		{"gastown", "bead", "a bead was filed", "plain"},
		{"town", "landing", "a landing merged", "success"},
		{"longeye", "dispatch", "a dispatch failed", "failure"},
	}
	for i, l := range lines {
		e := activityEntry(i+1, now.Add(time.Duration(i)*time.Second), l.rig, l.kind, l.text, l.class)
		deliverStream(t, dom, "entry", mustJSONString(t, e))
	}

	rows := activityRows(t, dom)
	if len(rows) != len(lines) {
		t.Fatalf("three entry frames drew %d rows, want 3: %+v", len(rows), rows)
	}
	want := []struct{ rig, kind, text, class string }{lines[2], lines[1], lines[0]}
	for i, w := range want {
		at := now.Add(time.Duration(len(want)-1-i) * time.Second)
		cells := []string{pageExpr(t, dom, "clock("+quoteJS(at.Format(time.RFC3339))+")"), w.rig, w.kind, w.text}
		if !slices.Equal(rows[i].Cells, cells) {
			t.Errorf("row %d reads %q, want time, rig, kind and text %q", i, rows[i].Cells, cells)
		}
		if !strings.Contains(rows[i].Cls, "at-"+w.class) {
			t.Errorf("row %d is classed %q, want the line's %q class on it", i, rows[i].Cls, w.class)
		}
	}
}

// A backlog frame replaces what the page drew so far with the ring it carries,
// oldest first as the hub keeps it and newest first as the pane draws it, and a
// ring past the pane's keep leaves exactly the newest two hundred.
func TestActivityBacklogReplacesTheFeedAndKeepsTheNewestTwoHundred(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	now := pageNow(t, dom)
	for i := 0; i < 2; i++ {
		e := activityEntry(100+i, now.Add(-time.Hour), "gastown", "bead", fmt.Sprintf("drawn before the backlog %d", i), "plain")
		deliverStream(t, dom, "entry", mustJSONString(t, e))
	}

	// A backlog of three replaces the two lines the stream had drawn.
	var three []map[string]any
	for i := 0; i < 3; i++ {
		three = append(three, activityEntry(i+1, now.Add(time.Duration(i-3)*time.Minute), "town", "landing", fmt.Sprintf("backlog line %d", i), "plain"))
	}
	deliverStream(t, dom, "backlog", mustJSONString(t, three))
	rows := activityRows(t, dom)
	if len(rows) != 3 {
		t.Fatalf("a backlog of three replaced the feed with %d rows, want 3", len(rows))
	}
	if rows[0].Cells[3] != "backlog line 2" || rows[2].Cells[3] != "backlog line 0" {
		t.Errorf("the backlog's rows read %q to %q, want newest first from %q to %q",
			rows[0].Cells[3], rows[2].Cells[3], "backlog line 2", "backlog line 0")
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Cells[3], "drawn before the backlog") {
			t.Errorf("the feed still holds %q, which the backlog did not carry", r.Cells[3])
		}
	}

	// A ring of 250 keeps the newest 200, the oldest 50 dropped.
	var ring []map[string]any
	for i := 0; i < 250; i++ {
		ring = append(ring, activityEntry(i+1, now.Add(time.Duration(i-250)*time.Second), "gastown", "bead", fmt.Sprintf("line %d", i), "plain"))
	}
	deliverStream(t, dom, "backlog", mustJSONString(t, ring))
	rows = activityRows(t, dom)
	if len(rows) != 200 {
		t.Fatalf("a ring of 250 left %d rows, want 200", len(rows))
	}
	if rows[0].Cells[3] != "line 249" || rows[199].Cells[3] != "line 50" {
		t.Errorf("the feed runs from %q to %q, want the newest 200 from %q to %q",
			rows[0].Cells[3], rows[199].Cells[3], "line 249", "line 50")
	}

	// A line the stream adds keeps the feed at 200 and drops the oldest.
	deliverStream(t, dom, "entry", mustJSONString(t, activityEntry(999, now, "town", "bead", "after the ring", "plain")))
	rows = activityRows(t, dom)
	if len(rows) != 200 {
		t.Fatalf("a line after a full ring left %d rows, want 200", len(rows))
	}
	if rows[0].Cells[3] != "after the ring" || rows[199].Cells[3] != "line 51" {
		t.Errorf("the feed after one more line runs from %q to %q, want %q to %q",
			rows[0].Cells[3], rows[199].Cells[3], "after the ring", "line 51")
	}
}

// A line's text goes in as text: markup characters in an entry draw as
// themselves, not as elements.
func TestActivityMarkupInALineDrawsAsText(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	now := pageNow(t, dom)
	text := `<b>bold</b> & <img src=x onerror=alert(1)>`
	deliverStream(t, dom, "entry", mustJSONString(t, activityEntry(1, now, "ri<g>", "be<ad>", text, "plain")))

	rows := activityRows(t, dom)
	if len(rows) != 1 {
		t.Fatalf("one entry frame drew %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].Cells[3] != text {
		t.Errorf("the text cell reads %q, want the line's own %q", rows[0].Cells[3], text)
	}
	if rows[0].Cells[1] != "ri<g>" || rows[0].Cells[2] != "be<ad>" {
		t.Errorf("the rig and kind cells read %q and %q, want the line's own words", rows[0].Cells[1], rows[0].Cells[2])
	}
}

// A line the page cannot place draws without guessing: a class it has never seen
// draws plain rather than as itself, a stamp it cannot read draws no time and
// gives the note no age, and a frame carrying no line draws nothing.
func TestActivityOddLinesDrawWithoutGuessing(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	now := pageNow(t, dom)
	deliverStream(t, dom, "entry", mustJSONString(t, activityEntry(1, now, "gastown", "bead", "an odd line", "bogus")))
	rows := activityRows(t, dom)
	if len(rows) != 1 {
		t.Fatalf("a line of an unknown class drew %d rows, want 1", len(rows))
	}
	if got := rows[0].Cls; !strings.Contains(got, "at-plain") || strings.Contains(got, "bogus") {
		t.Errorf("a line classed %q wears %q, want the plain colour and not the class itself", "bogus", got)
	}

	odd := map[string]any{"seq": 2, "at": "not a stamp", "rig": "gastown", "kind": "bead", "text": "undated", "class": "plain"}
	deliverStream(t, dom, "entry", mustJSONString(t, odd))
	rows = activityRows(t, dom)
	if len(rows) != 2 {
		t.Fatalf("an undated line drew %d rows, want 2", len(rows))
	}
	if got := rows[0].Cells[0]; got != "" {
		t.Errorf("an undated line draws the time %q, want none", got)
	}
	if got := activityHeaderNote(t, dom); got != "2 rows" {
		t.Errorf("the note reads %q over an undated newest line, want the count alone", got)
	}

	// A frame that carries no line, and a backlog with a hole in it, draw nothing.
	deliverStream(t, dom, "entry", "null")
	if n := len(activityRows(t, dom)); n != 2 {
		t.Errorf("a null entry frame left %d rows, want the 2 the feed held", n)
	}
	deliverStream(t, dom, "backlog", mustJSONString(t, []any{nil}))
	if n := len(activityRows(t, dom)); n != 0 {
		t.Errorf("a backlog holding only a null left %d rows, want none", n)
	}
}

// An empty feed says so in the body, before the first frame and after a backlog
// that carries nothing.
func TestActivityEmptyFeedSaysNoActivityInTheWindow(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if got := activityEmptyText(t, dom); got != "no activity in the window" {
		t.Errorf("an empty feed draws %q, want %q", got, "no activity in the window")
	}

	deliverStream(t, dom, "backlog", "[]")
	if got := activityEmptyText(t, dom); got != "no activity in the window" {
		t.Errorf("an empty backlog draws %q, want %q", got, "no activity in the window")
	}
	if n := len(activityRows(t, dom)); n != 0 {
		t.Errorf("an empty backlog drew %d rows, want none", n)
	}
}

// The pane starts shut, and its header still says what it holds: the line count
// and the age of the newest line, which the shut pane shows as its summary.
func TestActivityPaneStartsShutAndItsNoteCountsTheRows(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if !foldClosed(t, dom, "activity") {
		t.Fatal("the Town activity pane starts open, want it shut by default")
	}
	if got := activityHeaderNote(t, dom); got != "0 rows" {
		t.Errorf("the header reads %q before any line, want %q", got, "0 rows")
	}

	streamActivity(t, dom, 1, 0, "gastown", "bead", "one line", "plain")
	if got := activityHeaderNote(t, dom); got != "1 row · newest 0s ago" {
		t.Errorf("the header reads %q for one line just arrived, want %q", got, "1 row · newest 0s ago")
	}
	if got := activitySummary(t, dom); got != "1 row · newest 0s ago" {
		t.Errorf("the shut pane's summary reads %q, want the header's note", got)
	}

	// Two more lines, the newest of them two minutes old.
	for i := 0; i < 2; i++ {
		streamActivity(t, dom, i+2, time.Duration(3-i)*time.Minute, "town", "landing", fmt.Sprintf("line %d", i), "plain")
	}
	if got := activityHeaderNote(t, dom); got != "3 rows · newest 2m ago" {
		t.Errorf("the header reads %q for three lines two minutes old, want %q", got, "3 rows · newest 2m ago")
	}
}

// The newest line's age keeps moving on the 15 s tick: the clock is not part of
// any frame the page compares, so a note drawn once would hold the age it was
// drawn at.
func TestActivityNoteAgeMovesOnTheTick(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	// A state reading, so the page's tick has a state to redraw from; the fields
	// are the ones the panes the tick draws read.
	reading := `({rigs: [{name: "gastown", seats: 1, ready: 2, landing: 0}],
		queue: {at: new Date(Date.now()).toISOString()},
		reads: {queue: {at: new Date(Date.now()).toISOString()}}})`
	if _, err := dom.vm.RunString(`__fire(__sources[` + liveStream + `], "state", ` + reading + ")"); err != nil {
		t.Fatalf("delivering a state reading: %v", err)
	}
	streamActivity(t, dom, 1, 0, "gastown", "bead", "a line", "plain")
	if got := activityHeaderNote(t, dom); got != "1 row · newest 0s ago" {
		t.Fatalf("the header reads %q for a line just arrived, want %q", got, "1 row · newest 0s ago")
	}

	advanceTimers(t, dom, 15000)
	if got := activityHeaderNote(t, dom); got != "1 row · newest 15s ago" {
		t.Errorf("after the tick the header reads %q, want %q", got, "1 row · newest 15s ago")
	}
}
