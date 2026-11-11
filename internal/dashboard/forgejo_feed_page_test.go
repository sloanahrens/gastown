package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// feedCell is one cell of an event row the Forgejo pane drew: its class names
// the column (t, a, s, rp) and text is what the column shows.
type feedCell struct {
	Class string `json:"class"`
	Text  string `json:"text"`
}

// drawnFeedRows reads back every event row under the feed, each as its cells in
// the order the row draws them: time, actor, summary, repo. The feed's other
// lines (the empty line, the missing and failed repo notes) are not rows.
func drawnFeedRows(t *testing.T, dom *pageDOM) [][]feedCell {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify(__byId["forgejo"].children.__arr.filter(r => r.className === "fev").map(r => r.children.__arr.map(c => ({class: c.className, text: c.textContent}))))`)
	if err != nil {
		t.Fatalf("reading the Forgejo feed rows: %v", err)
	}
	var rows [][]feedCell
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the Forgejo feed rows: %v", err)
	}
	return rows
}

// feedEvent is one event in the shape the feed serves it: the stamp is the
// time the event happened, the rest is what the row shows.
func feedEvent(at time.Time, actor, summary, repo string) map[string]any {
	return map[string]any{"at": at.UTC().Format(time.RFC3339Nano), "actor": actor, "summary": summary, "repo": repo}
}

// The pane lists each event it was served as one row: the time, the actor, the
// summary and the repo, in that order, and nothing it was not served.
func TestForgejoFeedDrawsOneRowPerEventWithItsFields(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// Seconds back, not minutes: the pane stamps an event from today with a
	// clock time and an older one with a date, and a three-minute offset would
	// push the first event over midnight on a run that starts just after it.
	events := []any{
		feedEvent(now.Add(-3*time.Second), "bot-polecat", "push to main", "gastown"),
		feedEvent(now.Add(-2*time.Second), "bot-landing", "delete branch land/gt-1", "beads"),
		feedEvent(now.Add(-time.Second), "sloan", "open issue", "longeye"),
	}
	dom := newPageDOM(t)
	dom.call(t, "renderForgejo", map[string]any{"forgejo": map[string]any{
		"at": now.UTC().Format(time.RFC3339Nano), "every_seconds": 300, "events": events,
	}})

	rows := drawnFeedRows(t, dom)
	if len(rows) != len(events) {
		t.Fatalf("drew %d rows for %d events: %+v", len(rows), len(events), rows)
	}
	want := []struct{ actor, summary, repo string }{
		{"bot-polecat", "push to main", "gastown"},
		{"bot-landing", "delete branch land/gt-1", "beads"},
		{"sloan", "open issue", "longeye"},
	}
	for i, row := range rows {
		if len(row) != 4 {
			t.Fatalf("row %d has %d cells, want time, actor, summary and repo: %+v", i, len(row), row)
		}
		for j, class := range []string{"t", "a", "s", "rp"} {
			if row[j].Class != class {
				t.Errorf("row %d cell %d is class %q, want %q", i, j, row[j].Class, class)
			}
		}
		if !strings.Contains(row[0].Text, ":") {
			t.Errorf("row %d shows %q for its time, want a clock time", i, row[0].Text)
		}
		if row[1].Text != want[i].actor || row[2].Text != want[i].summary || row[3].Text != want[i].repo {
			t.Errorf("row %d shows %q %q %q, want %q %q %q", i,
				row[1].Text, row[2].Text, row[3].Text, want[i].actor, want[i].summary, want[i].repo)
		}
	}
}

// With no events the pane says so in words, and draws no row.
func TestForgejoFeedWithNoEventsSaysSo(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderForgejo", map[string]any{"forgejo": map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339Nano), "every_seconds": 300, "events": []any{},
	}})

	if rows := drawnFeedRows(t, dom); len(rows) != 0 {
		t.Fatalf("drew %d rows for no events: %+v", len(rows), rows)
	}
	line := dom.descend(t, "forgejo", 0)
	if line.Text != "no activity" {
		t.Fatalf("the empty feed reads %q, want %q", line.Text, "no activity")
	}
	if line.Class != "empty" {
		t.Errorf("the empty feed is drawn as class %q, want the muted empty line", line.Class)
	}
}

// The count the header carries is the same in both states of the section: the
// closed header shows it as the summary, and the open header shows it as the
// note beside the title, and the feed's own header shows it when the feed folds.
func TestForgejoPaneHeaderKeepsTheEventCountInEveryState(t *testing.T) {
	t.Parallel()

	now := time.Now()
	var events []any
	for i := 0; i < 3; i++ {
		events = append(events, feedEvent(now.Add(-time.Duration(i+1)*time.Minute), "a", "s", "r"))
	}
	state := map[string]any{"forgejo": map[string]any{
		"at": now.UTC().Format(time.RFC3339Nano), "every_seconds": 300, "events": events,
		"actions": map[string]any{
			"current": []any{}, "recent": []any{},
			"stats": map[string]any{"running": 0, "queued": 0, "ok": 0, "failed": 0, "median_secs": 0},
		},
	}}

	t.Run("collapsed section", func(t *testing.T) {
		t.Parallel()
		dom := newPageDOM(t)
		drawForgejoState(t, dom, state)

		v, err := dom.vm.RunString(`paneFold("forgejo").closed + "|" + paneFold("forgejo").el.querySelector(":scope > h2").querySelector(".foldsum").textContent`)
		if err != nil {
			t.Fatalf("reading the collapsed Forgejo header: %v", err)
		}
		closed, text, _ := strings.Cut(v.String(), "|")
		if closed != "true" {
			t.Fatalf("the Forgejo section is not collapsed on a fresh load")
		}
		if !strings.Contains(text, "3 events") {
			t.Errorf("the collapsed header reads %q, want the event count", text)
		}
	})

	t.Run("expanded section", func(t *testing.T) {
		t.Parallel()
		dom := newPageDOM(t)
		openForgejoSection(t, dom)
		drawForgejoState(t, dom, state)

		if note := dom.element(t, "forgejonote").Text; !strings.Contains(note, "3 events") {
			t.Errorf("the expanded header reads %q, want the event count", note)
		}
		if rows := drawnFeedRows(t, dom); len(rows) != 3 {
			t.Errorf("the expanded section drew %d rows, want 3", len(rows))
		}
	})

	t.Run("folded feed", func(t *testing.T) {
		t.Parallel()
		dom := newPageDOMStoring(t, map[string]string{"gt-forgejo-feed": "folded"})
		openForgejoSection(t, dom)
		drawForgejoState(t, dom, state)

		head := dom.element(t, "forgejofeedhead")
		if !strings.Contains(head.Text, "3 events") {
			t.Errorf("the folded feed header reads %q, want the event count", head.Text)
		}
	})
}

// drawForgejoState pushes state through the page the way the server's reading
// arrives, then paints the folds so a collapsed header shows its summary.
func drawForgejoState(t *testing.T, dom *pageDOM, state map[string]any) {
	t.Helper()

	b, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encoding the Forgejo reading: %v", err)
	}
	if _, err := dom.vm.RunString(fmt.Sprintf("state = %s; renderAll(); paintFolds(state);", b)); err != nil {
		t.Fatalf("drawing the Forgejo pane: %v", err)
	}
}

// openForgejoSection opens the Forgejo section the way a reader does, by its
// heading, so the section draws its body the way it does on a click.
func openForgejoSection(t *testing.T, dom *pageDOM) {
	t.Helper()

	if _, err := dom.vm.RunString(`(function () {
		const h = paneFold("forgejo").el.querySelector(":scope > h2");
		__fireOn(h, "click", {type: "click", target: h});
	})();`); err != nil {
		t.Fatalf("opening the Forgejo section: %v", err)
	}
}
