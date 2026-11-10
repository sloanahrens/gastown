package dashboard

import (
	"strings"
	"testing"
	"time"
)

// Every pane that keeps a last good value reads its own source's status, greys
// what it is showing and colours its caption, so a failed or aged read is never
// drawn as a current, empty one (gt-q6h8e).
func TestThePageMarksAFailedOrStaleReading(t *testing.T) {
	t.Parallel()

	for _, pane := range []struct{ fn, source, note string }{
		{"renderPolecats", "seats", "polecatsnote"},
		{"renderBeads", "queue", "beadsnote"},
		{"renderMachine", "machine", "machnote"},
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

	// The Cost panel greys its spend lines (not the usage and GCP lines drawn
	// from other reads) and colours its caption with the spend read's status.
	cost := pageFunc(t, "renderCost")
	for _, want := range []string{`readOf(s, "spend")`, `paintsRead(spendBox, note, r);`} {
		if !strings.Contains(cost, want) {
			t.Errorf("renderCost does not grey the spend lines on a failed or aged read: no %q", want)
		}
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
	// return, where the seat ages move in place.
	for _, pane := range []struct{ fn, note, key string }{
		{"renderPolecats", `$("polecatsnote").textContent`, "key === pKey)"},
		{"renderBeads", `$("beadsnote").textContent`, "key === bKey)"},
	} {
		body := pageFunc(t, pane.fn)
		note, skip := strings.Index(body, pane.note), strings.Index(body, pane.key)
		if note < 0 || skip < 0 || note > skip {
			t.Errorf("%s writes its ages below the early return, so they freeze (note=%d skip=%d)", pane.fn, note, skip)
		}
	}
	if !strings.Contains(string(indexHTML), "renderStrip(state); renderPolecats(state); renderBeads(state); renderTierSweep(state);") {
		t.Error("the 15s tick does not redraw the Polecats and Beads notes, so their ages still freeze")
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

// A failed read names its reason where the operator reads that pane: the
// polecats note, the strip's tips for waiting to land and needs you, and the
// attention panel's line. The reason is drawn as text, so markup in it stays
// literal, and the page holds it to 200 characters (gt-6qc3nw.2).
func TestPaneTipShowsWhy(t *testing.T) {
	t.Parallel()

	markup := "<b>dolt</b> circuit breaker " + strings.Repeat("x", 300)
	want := string([]rune(markup)[:200])
	now := time.Now().UTC().Format(time.RFC3339)
	failed := map[string]any{"at": now, "every_sec": 60, "error": true, "stale": true}

	t.Run("polecats note", func(t *testing.T) {
		t.Parallel()
		dom := newPageDOM(t)
		dom.call(t, "renderPolecats", map[string]any{
			"summary":    map[string]any{"seats_why": markup},
			"summary_at": now,
			"polecats":   []any{},
			"reads":      map[string]any{"seats": failed},
		})
		note := dom.element(t, "polecatsnote").Text
		if !strings.Contains(note, "the seats read failed: "+want) {
			t.Errorf("the polecats note reads %q, want the seats' reason held to 200 characters", note)
		}
		if strings.Contains(note, markup) {
			t.Errorf("the polecats note draws the reason past 200 characters: %q", note)
		}
	})

	t.Run("strip", func(t *testing.T) {
		t.Parallel()
		dom := newPageDOM(t)
		dom.call(t, "renderStrip", map[string]any{
			"summary":   map[string]any{"ready_why": markup},
			"reads":     map[string]any{"ready": failed},
			"attention": map[string]any{"rows": []any{}, "failed": []any{"questions"}},
			"questions": map[string]any{"unavailable": true, "why": markup},
		})
		waiting := stripFact(t, dom, "waiting to land").Title
		if !strings.Contains(waiting, "the read that lists what is waiting to land failed: "+want) {
			t.Errorf("the waiting-to-land tip reads %q, want the ready read's reason", waiting)
		}
		needs := stripFact(t, dom, "needs you").Title
		if !strings.Contains(needs, "the questions could not be read: "+want) {
			t.Errorf("the needs-you tip reads %q, want the questions' reason", needs)
		}
		if !strings.Contains(needs, "<b>dolt</b>") {
			t.Errorf("the needs-you tip reads %q, want the reason's markup drawn as text", needs)
		}
	})

	t.Run("attention", func(t *testing.T) {
		t.Parallel()
		dom := newPageDOM(t)
		dom.call(t, "renderAttention", map[string]any{
			"attention": map[string]any{"rows": []any{}, "failed": []any{"questions"}},
			"questions": map[string]any{"unavailable": true, "why": markup},
		})
		text := dom.element(t, "attention").Text
		if !strings.Contains(text, "unavailable: the questions could not be read: "+want) {
			t.Errorf("the attention panel reads %q, want the questions' reason", text)
		}
		if strings.Contains(text, markup) {
			t.Errorf("the attention panel draws the reason past 200 characters: %q", text)
		}
	})
}
