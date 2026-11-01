package dashboard

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// A push draws a pane only when the reading it is drawn from changed since its
// last draw, and leaves a closed section's body for the reader to open
// (gt-quuq5). These tests deliver the state on the page's own stream — the way
// the server pushes it — so what runs the page's guard is renderAll, not the
// direct render call the other page tests use to draw one pane.

// pushState delivers one state reading on the live stream. The stub's __fire
// stringifies its argument into the event's data, so the reading goes in as a
// JavaScript value rather than as JSON text it would escape a second time.
func pushState(t *testing.T, dom *pageDOM, state map[string]any) {
	t.Helper()

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encoding the reading: %v", err)
	}
	if _, err := dom.vm.RunString("__fire(__sources[" + liveStream + `], "state", (` + string(data) + "))"); err != nil {
		t.Fatalf("delivering a state reading: %v", err)
	}
}

// countPaneDraws counts the draws of one pane by its body's replaceChildren —
// the first thing every draw of a body does — and answers a function that reads
// the count. The element is the stub's own, so this counts what the page did to
// the element it draws into, not what a function was called with.
func countPaneDraws(t *testing.T, dom *pageDOM, id string) func() int {
	t.Helper()

	if _, err := dom.vm.RunString(`(function () {
		var box = __byId[` + strconv.Quote(id) + `], n = 0, orig = box.replaceChildren;
		box.replaceChildren = function () { n++; return orig.apply(this, arguments); };
		window.__paneDraws = function () { return n; };
	})()`); err != nil {
		t.Fatalf("counting the draws of #%s: %v", id, err)
	}
	return func() int { return pageNumber(t, dom, "__paneDraws()") }
}

// attentionReading is one reading of the attention list: the panel's one row,
// with a title the test moves to make a field the pane reads change.
func attentionReading(title string) map[string]any {
	return map[string]any{"attention": map[string]any{"rows": []any{
		map[string]any{"owner": "operator", "kind": "question", "class": "needs", "title": title},
	}}}
}

func TestPageDrawsAPaneOnlyWhenItsReadingChanged(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if dom.fold(t, "attention").Collapsed {
		t.Fatal("the attention section starts open for a reader with no saved choice, so its body is what a push draws")
	}
	draws := countPaneDraws(t, dom, "attention")

	pushState(t, dom, attentionReading("is the dome open"))
	if got := draws(); got != 1 {
		t.Fatalf("the first reading drew #attention %d times, want once", got)
	}
	// The same reading again: nothing the panel draws from moved, so it is left
	// as it is — this is the draw the bead removed from every push.
	pushState(t, dom, attentionReading("is the dome open"))
	if got := draws(); got != 1 {
		t.Errorf("the same reading drew #attention %d times, want the one draw it already had", got)
	}
	// A field the panel reads moved, so the push draws it again.
	pushState(t, dom, attentionReading("the dome is open"))
	if got := draws(); got != 2 {
		t.Errorf("a changed reading drew #attention %d times, want 2", got)
	}
	// The change reached the body the reader is looking at.
	if text := dom.element(t, "attention").Text; !strings.Contains(text, "the dome is open") {
		t.Errorf("#attention reads %q, want the changed row's title", text)
	}
}

// rigsReading is one reading of the rigs table: n rigs, each named after its
// index, so a body drawn from it is recognisable.
func rigsReading(n int) map[string]any {
	rows := make([]any, 0, n)
	for i := 1; i <= n; i++ {
		rows = append(rows, map[string]any{"name": "rig-" + strconv.Itoa(i), "seats": 1, "ready": 2, "landing": 0})
	}
	return map[string]any{"rigs": rows}
}

func TestPageLeavesAClosedSectionsBodyUntilItIsOpened(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if !dom.fold(t, "rigs").Collapsed {
		t.Fatal("the rigs section starts closed for a reader with no saved choice")
	}
	draws := countPaneDraws(t, dom, "rigs")

	pushState(t, dom, rigsReading(2))
	if got := dom.element(t, "rigs").Kids; got != 0 {
		t.Fatalf("a push drew the closed rigs section's body: %d children", got)
	}
	// The heading's note is drawn whatever the fold says: it is the line a
	// closed section shows.
	if got, want := dom.fold(t, "rigs").Summary, "2 rigs"; got != want {
		t.Fatalf("the closed rigs section reads %q, want %q", got, want)
	}

	pushState(t, dom, rigsReading(3))
	if got := dom.fold(t, "rigs").Summary; got != "3 rigs" {
		t.Errorf("the closed rigs section reads %q after a second reading, want its summary to follow the reading", got)
	}
	if got := draws(); got != 0 {
		t.Fatalf("a closed section's body was replaced %d times, want none", got)
	}

	// Opening it draws the body the pushes left alone, from the reading the page
	// already holds: no fetch is made.
	fetches := pageNumber(t, dom, "__fetches.length")
	dom.press(t, "rigs", "click")
	if got := draws(); got != 1 {
		t.Errorf("opening the rigs section drew its body %d times, want once", got)
	}
	if text := dom.element(t, "rigs").Text; !strings.Contains(text, "rig-3") {
		t.Errorf("the opened rigs section reads %q, want the last reading's rigs", text)
	}
	if got := pageNumber(t, dom, "__fetches.length") - fetches; got != 0 {
		t.Errorf("opening the rigs section sent %d request(s), want none", got)
	}
}

// A note's age keeps moving on the 15 s tick: the clock is not part of the
// reading a push compares, so a closed section's summary line would otherwise
// hold the age it was drawn at (gt-quuq5).
func TestPageTickKeepsAClosedSectionsAgeMoving(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if !dom.fold(t, "rigs").Collapsed {
		t.Fatal("the rigs section starts closed for a reader with no saved choice")
	}
	// The read is stamped now on the page's own clock, so its age is the time the
	// tick moves rather than a wall clock the test cannot hold still.
	reading := `({rigs: [{name: "gastown", seats: 1, ready: 2, landing: 0}],
		queue: {at: new Date(Date.now()).toISOString()},
		reads: {queue: {at: new Date(Date.now()).toISOString()}}})`
	if _, err := dom.vm.RunString(`__fire(__sources[` + liveStream + `], "state", ` + reading + ")"); err != nil {
		t.Fatalf("delivering a state reading: %v", err)
	}
	if got, want := dom.fold(t, "rigs").Summary, "1 rigs · read 0s ago"; got != want {
		t.Fatalf("the closed rigs section reads %q, want %q", got, want)
	}

	advanceTimers(t, dom, 15000)
	if got, want := dom.fold(t, "rigs").Summary, "1 rigs · read 15s ago"; got != want {
		t.Errorf("after the tick the closed rigs section reads %q, want %q", got, want)
	}
}
