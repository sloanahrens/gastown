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
		{"renderSeats", "seats", "seatsnote"},
		{"renderMachine", "machine", "machnote"},
		{"renderSpend", "spend", "spendnote"},
		{"renderOM", "om", "omnote"},
		{"renderQueue", "queue", "queuenote"},
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

	// A failed seats read is unavailable, never a town with no polecats.
	if seats := pageFunc(t, "renderSeats"); !strings.Contains(seats, "unavailable — the seats read failed or went stale") {
		t.Error("renderSeats reads a failed seats read as a town with no polecats")
	}

	// Machine and OM carried no age at all: the reading could not be told fresh
	// from frozen.
	for _, name := range []string{"renderMachine", "renderOM"} {
		if body := pageFunc(t, name); !strings.Contains(body, `"read " + fmtDur(r.ageSec) + " ago"`) {
			t.Errorf("%s shows no age for its reading", name)
		}
	}

	// The queue's age is written on every call, above the unchanged-key early
	// return that used to freeze it between polls.
	queue := pageFunc(t, "renderQueue")
	note, skip := strings.Index(queue, `$("queuenote").textContent`), strings.Index(queue, "key === qKey) return;")
	if note < 0 || skip < 0 || note > skip {
		t.Errorf("renderQueue writes its age below the early return, so the age freezes (note=%d skip=%d)", note, skip)
	}
	if !strings.Contains(string(indexHTML), "renderTierSweep(state); renderForgejo(state); renderQueue(state);") {
		t.Error("the 15s tick does not redraw the queue's note, so its age still freezes")
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
