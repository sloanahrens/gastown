package dashboard

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
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

	dom := newPageDOMOpening(t, "attention")
	if dom.fold(t, "attention").Collapsed {
		t.Fatal("the attention section is open here, so its body is what a push draws")
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

// TestPageRedrawsAPaneWhenOnlyItsReadStatusChanged: a panel draws the read
// behind its numbers too — the caption's words and the grey — so a read that
// failed while the value it last returned stayed put is a change its draw must
// see (gt-q6h8e, gt-quuq5). The rigs pane reads one source and the Work pane
// two, so the one reading that moved is checked both ways.
func TestPageRedrawsAPaneWhenOnlyItsReadStatusChanged(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	fresh := map[string]any{"at": stamp, "every_sec": 60}
	failed := map[string]any{"at": stamp, "every_sec": 60, "error": true, "stale": true}

	t.Run("the rigs pane", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		if !dom.fold(t, "rigs").Collapsed {
			t.Fatal("the rigs section starts closed for a reader with no saved choice")
		}
		dom.press(t, "rigs", "click")
		draws := countPaneDraws(t, dom, "rigs")
		reading := func(status map[string]any) map[string]any {
			s := rigsReading(2)
			s["reads"] = map[string]any{"queue": status}
			return s
		}
		pushState(t, dom, reading(fresh))
		if got := draws(); got != 1 {
			t.Fatalf("the first reading drew #rigs %d times, want once", got)
		}
		// The same rigs and the same queue stamp: only the read behind them failed.
		pushState(t, dom, reading(failed))
		if got := draws(); got != 2 {
			t.Errorf("a failed read drew #rigs %d times, want 2: the value it returned did not move, the status did", got)
		}
		if got := dom.element(t, "rigsnote").Text; !strings.Contains(got, "read failed") {
			t.Errorf("#rigsnote reads %q, want the failed read named", got)
		}
	})

	t.Run("the Polecats pane", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		if dom.fold(t, "polecats").Collapsed {
			t.Fatal("the polecats section starts open")
		}
		// Every stamp is the test's, and the seat carries no age of its own, so
		// the readings below differ in one field: the status of the seats read.
		reading := func(seats map[string]any) map[string]any {
			s := workState([]any{seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", 0)}, workQueueReading(3, 0, 0, 0))
			s["summary_at"] = stamp
			s["queue"].(map[string]any)["at"] = stamp
			s["reads"].(map[string]any)["seats"] = seats
			s["reads"].(map[string]any)["queue"] = fresh
			return s
		}
		pushState(t, dom, reading(fresh))
		pushState(t, dom, reading(fresh))
		if got := dom.element(t, "polecatsnote").Text; strings.Contains(got, "failed") {
			t.Fatalf("#polecatsnote reads %q before any read failed", got)
		}
		pushState(t, dom, reading(failed))
		if got := dom.element(t, "polecatsnote").Text; !strings.Contains(got, "the seats read failed") {
			t.Errorf("#polecatsnote reads %q after the seats read failed, want it named", got)
		}
	})
}

// TestPageRetriesAPaneWhoseDrawThrew: a draw that threw has not drawn the
// reading, so the push that ran it must not record that reading as drawn — the
// next push has to draw the pane again (gt-quuq5).
func TestPageRetriesAPaneWhoseDrawThrew(t *testing.T) {
	t.Parallel()

	dom := newPageDOMOpening(t, "attention")
	draws := countPaneDraws(t, dom, "attention")
	pushState(t, dom, attentionReading("is the dome open"))
	if got := draws(); got != 1 {
		t.Fatalf("the first reading drew #attention %d times, want once", got)
	}
	// The next draw throws, from the first thing every draw of the body does, so
	// the pane keeps the reading it was drawn from.
	if _, err := dom.vm.RunString(`(function () {
		var box = __byId["attention"], orig = box.replaceChildren, thrown = false;
		box.replaceChildren = function () {
			if (!thrown) { thrown = true; throw new Error("a draw that threw"); }
			return orig.apply(this, arguments);
		};
	})()`); err != nil {
		t.Fatalf("arming a draw that throws: %v", err)
	}
	data, err := json.Marshal(attentionReading("the dome is open"))
	if err != nil {
		t.Fatalf("encoding the reading: %v", err)
	}
	// The browser does not stop delivering because one push threw.
	if _, err := dom.vm.RunString(`try { __fire(__sources[` + liveStream + `], "state", (` + string(data) + `)); } catch (e) {}`); err != nil {
		t.Fatalf("delivering a reading whose draw throws: %v", err)
	}
	// The throw left the pane as it was: still on the old reading, and not
	// counted as drawn because the throw came before the body was touched.
	if got := dom.element(t, "attention").Text; !strings.Contains(got, "is the dome open") {
		t.Fatalf("#attention reads %q after a draw that threw, want the reading it already had", got)
	}
	pushState(t, dom, attentionReading("the dome is open"))
	if got := draws(); got != 2 {
		t.Fatalf("#attention was drawn %d times, want 2: the first reading and the retry", got)
	}
	if text := dom.element(t, "attention").Text; !strings.Contains(text, "the dome is open") {
		t.Errorf("#attention reads %q, want the reading the retried push drew", text)
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
