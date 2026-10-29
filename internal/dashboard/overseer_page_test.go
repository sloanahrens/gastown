package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Overseer panel is one section drawing the overseer's whole side of the
// town as a stack of named blocks — the heartbeat, the digest, the review's
// schedules, the dispatcher's tick and what waits on the overseer — so the
// blocks have no ids of their own and a test finds one by the name the panel
// gives it.

// overseerBlock is one block of the panel as the page drew it: the name its
// heading carries, the heading's note and the text under it.
type overseerBlock struct {
	Name string `json:"name"`
	Note string `json:"note"`
	Text string `json:"text"`
}

// overseerBlocks reads the blocks of #overseer in the order the panel drew
// them.
func overseerBlocks(t *testing.T, dom *pageDOM) []overseerBlock {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify(__byId["overseer"].children.map(b => ({
		name: b.children[0].children[0].textContent, note: b.children[0].children[1].textContent,
		text: b.children[1].textContent})))`)
	if err != nil {
		t.Fatalf("reading the panel's blocks: %v", err)
	}
	var out []overseerBlock
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the panel's blocks: %v", err)
	}
	return out
}

// blockNamed returns the block the panel drew under name, failing the test for
// a name the panel does not draw: a test naming a block that is gone checks
// nothing.
func blockNamed(t *testing.T, blocks []overseerBlock, name string) overseerBlock {
	t.Helper()

	for _, b := range blocks {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("the panel drew no %q block: %+v", name, blocks)
	return overseerBlock{}
}

// blockTable reads the first table one block drew, as rows of cell text: the
// Deep review block's rows are a table, and a test reads the cells rather than
// searching the block's text for them.
func blockTable(t *testing.T, dom *pageDOM, name string) [][]string {
	t.Helper()

	js := `JSON.stringify((() => {
		const b = __byId["overseer"].children.find(x => x.children[0].children[0].textContent === ` + strconv.Quote(name) + `);
		if (!b) return null;
		const t = b.children[1].children.find(c => c.tagName === "TABLE");
		if (!t) return null;
		return t.children.map(tr => tr.children.map(td => td.textContent));
	})())`
	v, err := dom.vm.RunString(js)
	if err != nil {
		t.Fatalf("reading the %s block's table: %v", name, err)
	}
	if v.String() == "null" {
		t.Fatalf("the %s block drew no table", name)
	}
	var out [][]string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the %s block's table: %v", name, err)
	}
	return out
}

// blockClasses reads the classes of one block's drawn children, in the order
// the block drew them, so a test can tell a warning that is drawn in the page's
// amber from one that is only words.
func blockClasses(t *testing.T, dom *pageDOM, name string) []string {
	t.Helper()

	js := `JSON.stringify((() => {
		const b = __byId["overseer"].children.find(x => x.children[0].children[0].textContent === ` + strconv.Quote(name) + `);
		if (!b) return null;
		return b.children[1].children.map(c => c.className);
	})())`
	v, err := dom.vm.RunString(js)
	if err != nil {
		t.Fatalf("reading the %s block's classes: %v", name, err)
	}
	if v.String() == "null" {
		t.Fatalf("the panel drew no %q block", name)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the %s block's classes: %v", name, err)
	}
	return out
}

// The panel is the overseer's side of the town in one panel, so it draws its
// blocks in the order that side reads in: what the overseer is doing, what the
// town did, what the review is doing, what the dispatcher is doing, and what
// waits on the overseer (gt-uv08f).
func TestOverseerPanelDrawsItsBlocksInOrder(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{})
	var names []string
	for _, b := range overseerBlocks(t, dom) {
		names = append(names, b.Name)
	}
	want := []string{"Heartbeat", "Digest", "Deep review", "Dispatcher", "For the overseer"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("the panel drew %v, want %v", names, want)
	}

	// The page draws it once, with every other panel: the four panels this one
	// replaces are not drawn at all.
	all := pageFunc(t, "renderAll")
	for _, gone := range []string{"renderReports(state)", "renderDeepReview(state)", "renderDispatch(state)"} {
		if strings.Contains(all, gone) {
			t.Errorf("renderAll still calls %s", gone)
		}
	}
	if n := strings.Count(all, "renderOverseer(state)"); n != 1 {
		t.Errorf("renderAll draws the Overseer panel %d times, want once", n)
	}
}

// The panel's note is the two readings on it that go stale — the overseer's
// standing and how long ago the dispatcher ticked — and the dispatcher's tick
// is the reading the daemon's dispatcher-silence watcher already dates a pane
// by, so it stays in the note rather than moving into the block.
func TestOverseerPanelNoteCarriesTheStandingAndTheTick(t *testing.T) {
	t.Parallel()

	tick := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{
		"overseer": map[string]any{"status": "alive", "silent_after_sec": 5400, "last_active_sec": 120},
		"dispatch": map[string]any{"at": tick},
	})
	note := dom.element(t, "overseernote")
	if !strings.Contains(note.Text, "alive") || !strings.Contains(note.Text, "last tick") {
		t.Errorf("the note reads %q, want the standing and the tick", note.Text)
	}
	if note.Class != "r" {
		t.Errorf("the note wears %q with a live overseer and a fresh tick, want the plain note", note.Class)
	}

	// A silent overseer is the amber the page marks every stale reading with,
	// and the words are the reader's own window.
	dom = newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{
		"overseer": map[string]any{"status": "silent", "silent_after_sec": 5400, "last_active_sec": 9000},
	})
	note = dom.element(t, "overseernote")
	if !strings.Contains(note.Text, "silent window 1h30m") || !strings.Contains(note.Class, "warnc") {
		t.Errorf("a silent overseer reads %q wearing %q", note.Text, note.Class)
	}

	// A dispatcher that stopped is the alert red: it ticks every minute, so the
	// note the watcher dates a pane by says so.
	dom = newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{
		"overseer": map[string]any{"status": "alive", "silent_after_sec": 5400, "last_active_sec": 120},
		"dispatch": map[string]any{"at": time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)},
	})
	note = dom.element(t, "overseernote")
	if !strings.Contains(note.Text, "last tick 20m ago") || !strings.Contains(note.Class, "badc") {
		t.Errorf("a stopped dispatcher reads %q wearing %q", note.Text, note.Class)
	}
}

// The heartbeat block is drawn from the state the reader decided: an unreadable
// read is named rather than drawn as a silent overseer, and the status word is
// the reader's own.
func TestOverseerHeartbeatDrawsTheStateTheReaderDecided(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "drawHeartbeat")
	for _, want := range []string{
		`const o = s.overseer;`,
		`box.append(el("div", "badc", "unavailable: the overseer's heartbeat or journal could not be read"))`,
		`const alive = o.status === "alive";`,
		`el("b", alive ? "good" : "warnc", o.status === "none" ? "no heartbeat" : o.status)`,
		`"last active " + fmtDur(o.last_active_sec) + " ago"`,
		`"has never beaten"`,
		`"last 7 days: " + counts.map(c => c.kind + " " + c.count).join(" · ")`,
		`o.open_questions != null`,
		`const lines = o.journal || [];`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("drawHeartbeat has no %q", want)
		}
	}
	// The journal text is the overseer's own, so every cell goes through el(),
	// which writes textContent: a tag in it shows as that text, never markup.
	if strings.Contains(draw, "innerHTML") {
		t.Error("drawHeartbeat sets markup from the journal's text")
	}

	dom := newPageDOM(t)
	age := int64(120)
	dom.call(t, "renderOverseer", map[string]any{"overseer": map[string]any{
		"status": "alive", "state": "working", "last_active_sec": age, "silent_after_sec": 5400,
		"interventions": []any{map[string]any{"kind": "hold", "count": 3}},
		"journal":       []any{map[string]any{"age_sec": 30, "kind": "hold", "text": "parked gt-abc", "bead": "gt-abc"}},
	}})
	block := blockNamed(t, overseerBlocks(t, dom), "Heartbeat")
	for _, want := range []string{"alive", "last active 2m ago", "working", "last 7 days: hold 3", "parked gt-abc"} {
		if !strings.Contains(block.Text, want) {
			t.Errorf("the heartbeat block reads %q, want %q in it", block.Text, want)
		}
	}
}

// The rows the overseer owes a move on are the attention list's own rows, drawn
// with the row the Attention panel draws them with: the panel opens on the
// human's rows, so the overseer reads its own here without switching that
// panel's chips (gt-bi6w0, gt-uv08f).
func TestOverseerQueueDrawsTheRowsTheOverseerOwns(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{"attention": map[string]any{"rows": []any{
		map[string]any{"kind": "item", "owner": "overseer", "rule": "landing.risk_path", "id": "risk:gt-abc:deadbeef",
			"title": "gt-abc landed deadbeef touching internal/land; overseer review wanted", "level": "crit", "age_sec": 7200},
		map[string]any{"kind": "item", "owner": "overseer", "rule": "dispatch.idle", "id": "dispatch:idle",
			"title": "5 idle ticks with 2 beads waiting", "level": "warn"},
		map[string]any{"kind": "question", "owner": "operator", "id": "gt-q1", "title": "retry policy", "state": "awaiting"},
	}}})

	block := blockNamed(t, overseerBlocks(t, dom), "For the overseer")
	if block.Note != "2 waiting" {
		t.Errorf("the block's note reads %q, want the two rows it holds", block.Note)
	}
	if !strings.Contains(block.Text, "overseer review wanted") || !strings.Contains(block.Text, "dispatch.idle") {
		t.Errorf("the block does not draw the overseer's rows: %q", block.Text)
	}
	// The human's row is the Attention panel's: showing it here as well would
	// be one fact in two panels.
	if strings.Contains(block.Text, "retry policy") {
		t.Errorf("the block draws a row the human owns: %q", block.Text)
	}

	// Nothing waiting is one line, and a reader that has not reported is a
	// named state, not the same thing.
	dom = newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{"attention": map[string]any{"rows": []any{}}})
	if got := blockNamed(t, overseerBlocks(t, dom), "For the overseer"); !strings.Contains(got.Text, "nothing is waiting on the overseer") {
		t.Errorf("an empty list reads %q", got.Text)
	}
	dom = newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{})
	if got := blockNamed(t, overseerBlocks(t, dom), "For the overseer"); !strings.Contains(got.Text, "reading…") {
		t.Errorf("a reader that has not reported reads %q", got.Text)
	}
}

// The state the page reads carries the panel: /api/state serves what the hub
// holds, and a panel nobody configured is absent rather than empty.
func TestOverseerReachesTheAPI(t *testing.T) {
	t.Parallel()

	open := 2
	age := int64(120)
	h := NewHub(Config{
		Now: func() time.Time { return questionPageNow },
		Overseer: func() *Overseer {
			return &Overseer{
				Status: OverseerAlive, State: "working", LastActiveSec: &age, SilentAfterSec: 5400,
				Journal:       []OverseerLine{{AgeSec: 30, Kind: "hold", Text: "parked gt-abc", Bead: "gt-abc"}},
				Interventions: []OverseerCount{{Kind: "hold", Count: 3}},
				OpenQuestions: &open,
			}
		},
	})
	h.pollOverseer()

	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"overseer"`, `"status":"alive"`, `"parked gt-abc"`, `"open_questions":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("/api/state has no %s: %s", want, body)
		}
	}

	bare := NewHub(Config{Now: func() time.Time { return questionPageNow }})
	if bare.State().Overseer != nil {
		t.Error("a hub with no Overseer reader holds a panel")
	}
}
