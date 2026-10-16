package dashboard

import (
	"strings"
	"testing"
)

// Every pane that keeps a last good value reads its own source's status, greys
// what it is showing and colours its caption, so a failed or aged read is never
// drawn as a current, empty one (gt-q6h8e).
func TestThePageMarksAFailedOrStaleReading(t *testing.T) {
	t.Parallel()

	for _, pane := range []struct{ fn, source, note string }{
		{"renderWork", "seats", "worknote"}, // the Work panel is joined from the seats and queue reads
		{"renderMachine", "machine", "machnote"},
		{"renderSpend", "spend", "spendnote"},
		{"renderOM", "om", "omnote"},
		{"renderRigs", "queue", "rigsnote"}, // the Rigs rows are joined from the queue read
	} {
		body := pageFunc(t, pane.fn)
		if want := `readOf(s, "` + pane.source + `")`; !strings.Contains(body, want) {
			t.Errorf("%s does not read its own source's status (%s)", pane.fn, want)
		}
		if want := `paintsRead(box, $("` + pane.note + `"), `; !strings.Contains(body, want) {
			t.Errorf("%s does not grey the pane and colour its caption: %s", pane.fn, want)
		}
	}

	// The Work panel draws the seats and the queue, so it reads both sources
	// and wears the worse of the two.
	work := pageFunc(t, "renderWork")
	if !strings.Contains(work, `readOf(s, "queue")`) {
		t.Error("renderWork does not read the queue's status, so half its pane is never greyed")
	}
	if !strings.Contains(work, "paintsRead(box, $(\"worknote\"), worseRead(rSeats, rQueue));") {
		t.Error("renderWork does not wear the worse of the two readings it joined")
	}

	// A failed seats read is unavailable, never a town with no polecats.
	if seats := pageFunc(t, "workSeats"); !strings.Contains(seats, "unavailable — the seats read failed or went stale") {
		t.Error("workSeats reads a failed seats read as a town with no polecats")
	}

	// Machine and OM carried no age at all: the reading could not be told fresh
	// from frozen.
	for _, name := range []string{"renderMachine", "renderOM"} {
		if body := pageFunc(t, name); !strings.Contains(body, `"read " + fmtDur(r.ageSec) + " ago"`) {
			t.Errorf("%s shows no age for its reading", name)
		}
	}

	// The panel's ages are written on every call, above the unchanged-key early
	// return that used to freeze them between polls.
	workNote := pageFunc(t, "renderWork")
	note, skip := strings.Index(workNote, `$("worknote").textContent`), strings.Index(workNote, "key === wKey) return;")
	if note < 0 || skip < 0 || note > skip {
		t.Errorf("renderWork writes its ages below the early return, so they freeze (note=%d skip=%d)", note, skip)
	}
	if !strings.Contains(string(indexHTML), "renderStrip(state); renderWork(state); renderTierSweep(state);") {
		t.Error("the 15s tick does not redraw the Work panel's note, so its ages still freeze")
	}

	// The page writes text, never markup, wherever a reading is described.
	if strings.Contains(string(indexHTML), "innerHTML") {
		t.Error("the page sets markup from a reading's text")
	}

	// One helper reads the status, and tolerates a server that sends none.
	helper := pageFunc(t, "readOf")
	for _, want := range []string{`s.reads[source]`, "x.error", "x.stale", "every_sec", "if (!x) return null;"} {
		if !strings.Contains(helper, want) {
			t.Errorf("readOf has no %q, so a server that sends no status breaks the page", want)
		}
	}
}
