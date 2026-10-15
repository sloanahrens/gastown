package dashboard

import (
	"strings"
	"testing"
)

// The strip is the whole of the page's top: the day's landings, the seats, the
// polecats on the job, what is waiting to land and what needs the operator,
// each drawn once and each off the reader that owns it, so the page cannot hold
// two pictures of one number (gt-ywlei).
func TestTheStripDrawsEachFactOnce(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderStrip")
	for _, want := range []string{
		// The landings off the Landings pane's own window.
		`const t = s.trend || null;`,
		`fact(box, t ? String(landed) : "–", "landed/24h",`,
		// The seats off the polecats the Polecats pane counts, and the queue
		// off the read that lists it.
		`seatFacts(s.models)`,
		`fact(box, s.summary ? String(active) : "–", "working",`,
		`const rr = readOf(s, "ready");`,
		`fact(box, q == null ? "–" : String(q), "waiting to land",`,
		`oldest == null ? null : "oldest " + fmtDur(oldest)`,
		// What needs the operator, off the two panes that own those rows.
		`const esc = paneCount(s.escalations), ask = paneCount(s.questions);`,
		`fact(box, String(esc + ask), "needs you",`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderStrip has no %q", want)
		}
	}

	// Every fact is one drawing of a number the panes below also count, so the
	// strip is the only thing here that draws it: the tiles and the header's
	// own models text are gone, and the page draws the strip once per push.
	all := pageFunc(t, "renderAll")
	if !strings.Contains(all, "renderHealth(state.health); renderStrip(state); renderModelsPanel(state);") {
		t.Error("the page's redraw does not draw the one strip")
	}
	if strings.Contains(all, "renderTiles(state)") {
		t.Error("the page's redraw still draws the tiles the strip replaced")
	}
	if strings.Contains(string(indexHTML), "renderModels(") {
		t.Error("the page still draws the header's own models text")
	}
	if n := strings.Count(all, "renderStrip(state)"); n != 1 {
		t.Errorf("renderAll draws the strip %d times, want 1", n)
	}
	// The ages in the strip drift between pushes, so the 15s tick redraws it
	// with the panes whose ages it carries.
	if !strings.Contains(string(indexHTML), "renderStrip(state); renderSeats(state); renderTierSweep(state);") {
		t.Error("the 15s tick does not redraw the strip, so its ages freeze")
	}
}

// The strip's seats are the open ones: a seat nothing may fill is left to the
// models line, a seat nobody could read says unknown rather than passing as a
// seat nobody runs, and the pro seat is named the reserved one — which is what
// the town keeps it for (gt pool mix). A seat whose polecats have not been read
// shows no count (gt-ywlei).
func TestTheStripNamesTheOpenSeatsAndTheReservedOne(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "seatFacts")
	for _, want := range []string{
		// No rows is a settings read that has not come back, never a town with
		// no seats: the reader always sends the three.
		`if (!m || !(m.seat_rows || []).length) return null;`,
		`if (sr.unknown) {`,
		`" seat unknown"`,
		`if (sr.closed) continue;`,
		`(sr.live == null ? "–" : sr.live)`,
		`(sr.seat === "pro" ? " (reserved)" : "")`,
		`sr.key || "no config key recorded"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("seatFacts has no %q", want)
		}
	}
	// No seat read at all is not a town with no seats, and no seat open is a
	// town running nothing: both are named rather than drawn as a count.
	strip := pageFunc(t, "renderStrip")
	for _, want := range []string{
		`if (!seats) { sp.append(el("b", "title", "–"), document.createTextNode(" seats"));`,
		`else if (!seats.length) { sp.append(el("b", "", "no open seats"));`,
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("renderStrip has no %q", want)
		}
	}
}

// A reading nobody has made is never drawn as a zero: the landings, the queue
// and the two panes behind "needs you" each say they could not be read, and a
// failed queue read is never a drained one (gt-ywlei, gt-q6h8e).
func TestTheStripNeverDrawsAReadingNobodyMade(t *testing.T) {
	t.Parallel()

	count := pageFunc(t, "paneCount")
	for _, want := range []string{"if (!pane) return null;", `if (pane.unavailable) return "unavailable";`, "(pane.rows || []).length + (pane.more || 0)"} {
		if !strings.Contains(count, want) {
			t.Errorf("paneCount has no %q", want)
		}
	}
	strip := pageFunc(t, "renderStrip")
	for _, want := range []string{
		`const landed = ((t && t.hours) || []).reduce((n, h) => n + h.landed, 0);`,
		`if (rr && rr.stale) fact(box, "unavailable", "waiting to land"`,
		`if (failed.length) fact(box, "unavailable", "needs you"`,
		`failed.join(" and ") + " could not be read"`,
		// A reading that has not come back is not a failed one: it reads as no
		// value yet, in the dim the page draws one it has never had.
		`else if (pending.length) fact(box, "–", "needs you", {cls: "title",`,
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("renderStrip has no %q", want)
		}
	}
}
