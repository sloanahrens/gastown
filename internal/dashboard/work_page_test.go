package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The Work panel is one section, so a test drives renderWork over the reading
// the server would send and reads back what it drew: the seat rows, the queue
// summary, the blocked line and the folded list. The panel replaced the
// Polecats and Work queue panels (gt-vcr3z), which drew the same beads twice.

// workState is one page state for the panel: the seats and the queue reading,
// with both read statuses fresh.
func workState(polecats []any, queue map[string]any) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	s := map[string]any{
		"summary":    map[string]any{"seats_cap": 4},
		"summary_at": now,
		"polecats":   polecats,
		"reads": map[string]any{
			"seats": map[string]any{"at": now, "every_sec": 60},
			"queue": map[string]any{"at": now, "every_sec": 60},
		},
	}
	if queue != nil {
		s["queue"] = queue
	}
	return s
}

// workRows reads every seat row the panel drew, folded or not, as the text of
// its cells. Every cell is a span — the hints a row carries are buttons in a
// div of their own — so a row reads as its five columns however much it
// carries.
func workRows(t *testing.T, dom *pageDOM) [][]string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const out = [];
		(function walk(n) {
			for (const c of (n.children || [])) {
				if (String(c.className).split(" ").indexOf("workrow") >= 0) {
					out.push(c.children.filter(x => x.tagName === "SPAN").map(x => x.textContent));
				}
				walk(c);
			}
		})(__byId["work"]);
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the panel's seat rows: %v", err)
	}
	var out [][]string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the panel's seat rows: %v", err)
	}
	return out
}

// workChild reads the first child of #work wearing class, as the text it
// reads and the classes beneath it.
func workChild(t *testing.T, dom *pageDOM, class string) drawn {
	t.Helper()

	js := `JSON.stringify((() => {
		const x = __byId["work"].children.find(c => String(c.className).split(" ").indexOf(` + quoteJS(class) + `) >= 0);
		return x ? {text: x.textContent, class: x.className, title: x.title, hidden: !!x.hidden, kids: x.children.length} : null;
	})())`
	v, err := dom.vm.RunString(js)
	if err != nil {
		t.Fatalf("reading #work's %s: %v", class, err)
	}
	if v.String() == "null" {
		t.Fatalf("the panel drew no %s", class)
	}
	var d drawn
	if err := json.Unmarshal([]byte(v.String()), &d); err != nil {
		t.Fatalf("reading #work's %s: %v", class, err)
	}
	return d
}

// quoteJS is a JavaScript string literal for s.
func quoteJS(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// seat is a polecat reading for the panel's tests.
func seat(rig, name, state, bead, title, agent string, slungAgo time.Duration) map[string]any {
	p := map[string]any{"rig": rig, "name": name, "state": state, "has_session": true}
	if bead != "" {
		p["bead"], p["title"], p["priority"] = bead, title, 3
	}
	if agent != "" {
		p["agent"] = agent
	}
	if slungAgo > 0 {
		at := time.Now().UTC().Add(-slungAgo).Format(time.RFC3339)
		p["slung"], p["last_active"] = at, at
	}
	return p
}

// workQueueReading is a queue reading: the three totals, the split of the
// blocked list, and one dispatchable ready bead.
func workQueueReading(ready, landing, blockedWork, blockedWisps int) map[string]any {
	blocked := []any{}
	for i := 0; i < blockedWork; i++ {
		blocked = append(blocked, map[string]any{"id": "gt-blocked" + string(rune('a'+i)), "title": "waits on another bead", "priority": 2})
	}
	for i := 0; i < blockedWisps; i++ {
		blocked = append(blocked, map[string]any{"id": "gt-wisp-" + string(rune('a'+i)), "title": "Self-review changes", "priority": 2, "wisp": true})
	}
	return map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339), "ready_total": ready, "landing_total": landing,
		"blocked_total": blockedWork + blockedWisps, "blocked_work": blockedWork, "blocked_wisps": blockedWisps,
		"ready":   []any{map[string]any{"id": "gt-a", "rig": "gastown", "title": "a bead", "priority": 2, "shape": "ok"}},
		"landing": []any{},
		"blocked": blocked,
	}
}

// TestTheWorkPanelDrawsARowPerSeat drives the panel over two seats — one
// holding a bead, one idle — and reads the rows back: the agent the session
// runs, the polecat, the bead with its title, the state and the bead's age,
// in that order, with the seats holding nothing folded under the ones that
// hold something (gt-vcr3z).
func TestTheWorkPanelDrawsARowPerSeat(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderWork", workState([]any{
		seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel replaces Polecats and Work queue", "deepseek-flash", 12*time.Minute),
		seat("gastown", "furiosa", "idle", "", "", "deepseek-flash", 0),
	}, workQueueReading(57, 1, 3, 118)))

	rows := workRows(t, dom)
	if len(rows) != 3 {
		t.Fatalf("the panel drew %d rows (%v), want a header and two seats", len(rows), rows)
	}
	if got := rows[0]; strings.Join(got, " ") != "Agent Polecat Bead State Age" {
		t.Errorf("the header row reads %v", got)
	}
	busy := rows[1]
	if len(busy) != 5 {
		t.Fatalf("a seat's row has %d cells (%v), want five", len(busy), busy)
	}
	if busy[0] != "deepseek-flash" {
		t.Errorf("the row does not name the agent its session runs: %v", busy)
	}
	if busy[1] != "dementus" {
		t.Errorf("the row does not name the polecat: %v", busy)
	}
	if !strings.Contains(busy[2], "gt-vcr3z") || !strings.Contains(busy[2], "one Work panel replaces") {
		t.Errorf("the row does not carry the bead and its title: %v", busy)
	}
	if busy[3] != "working" {
		t.Errorf("the row does not carry the seat's state: %v", busy)
	}
	if busy[4] != "12m" {
		t.Errorf("the row does not age the bead on the seat: %v", busy)
	}
	// The idle seat is drawn in the same row shape, folded under the seats that
	// hold something rather than left out.
	idle := rows[2]
	if len(idle) != 5 || idle[1] != "furiosa" || idle[3] != "idle" || idle[4] != "" {
		t.Errorf("the idle seat's row reads %v", idle)
	}
	if !strings.Contains(workText(t, dom), "1 idle (free to take work)") {
		t.Error("the idle seat is not folded under a summary that says what it is")
	}
}

// workText is the whole text the panel drew.
func workText(t *testing.T, dom *pageDOM) string {
	t.Helper()

	return dom.element(t, "work").Text
}

// TestTheWorkPanelSummarisesTheQueueAndSplitsTheBlockedList drives the panel
// over a queue of 7 dispatchable of 57 ready, 1 landing and 121 blocked — 3 of
// them real blocked beads and 118 leaked step wisps — and reads the summary and
// the blocked line back. The line separates the two and wears the page's
// warning colour on the wisps, which are not work anybody is blocked on
// (gt-vcr3z).
func TestTheWorkPanelSummarisesTheQueueAndSplitsTheBlockedList(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderWork", workState([]any{
		seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", time.Minute),
	}, workQueueReading(57, 1, 3, 118)))

	sum := workChild(t, dom, "wsum")
	for _, want := range []string{"1", "dispatchable", "57", "ready", "1", "landing"} {
		if !strings.Contains(sum.Text, want) {
			t.Errorf("the queue summary reads %q, want %q in it", sum.Text, want)
		}
	}

	line := workChild(t, dom, "wblocked")
	for _, want := range []string{"3", "beads blocked", "118", "leaked step wisps"} {
		if !strings.Contains(line.Text, want) {
			t.Errorf("the blocked line reads %q, want %q in it", line.Text, want)
		}
	}
	if !strings.Contains(line.Class, "warnc") {
		t.Errorf("a town with leaked step wisps draws the blocked line in %q, want the warning class", line.Class)
	}

	// A town with no wisps says so in the same line and wears no warning: the
	// count is the reading, not a standing alarm.
	dom = newPageDOM(t)
	dom.call(t, "renderWork", workState([]any{
		seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", time.Minute),
	}, workQueueReading(57, 1, 3, 0)))
	line = workChild(t, dom, "wblocked")
	if !strings.Contains(line.Text, "3 beads blocked") || strings.Contains(line.Text, "wisp") {
		t.Errorf("a town with no leaked wisps reads %q", line.Text)
	}
	if strings.Contains(line.Class, "warnc") {
		t.Errorf("the blocked line wears the warning colour with no wisps to warn about: %q", line.Class)
	}
}

// TestTheWorkPanelFoldsTheWholeQueueList drives the panel over a queue with
// blocked rows and reads the list back: it is one disclosure inside the panel,
// closed by default, and the beads the server marked as step wisps are the
// rows it hides — the same verdict the blocked line above it counted
// (gt-vcr3z).
func TestTheWorkPanelFoldsTheWholeQueueList(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderWork", workState([]any{
		seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", time.Minute),
	}, workQueueReading(57, 1, 2, 3)))

	list := workChild(t, dom, "wqueue")
	if list.Kids < 2 {
		t.Fatalf("the queue list is not a disclosure with a summary and a list: %+v", list)
	}
	open, err := dom.vm.RunString(`String(__byId["work"].children.find(c => String(c.className).indexOf("wqueue") >= 0).open)`)
	if err != nil {
		t.Fatalf("reading the list's fold: %v", err)
	}
	if open.String() != "false" {
		t.Errorf("the queue list starts open, want it folded")
	}
	if !strings.Contains(list.Text, "the full queue list") {
		t.Errorf("the folded list's summary reads %q", list.Text)
	}
}

// TestTheWorkPanelSaysOneLineBeforeTheTownHasBeenRead drives the panel with
// nothing read yet: a panel with nothing to show is its header and one short
// line, not a line per reader that has not come back (gt-vcr3z).
func TestTheWorkPanelSaysOneLineBeforeTheTownHasBeenRead(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderWork", map[string]any{})
	if kids, text := dom.element(t, "work").Kids, workText(t, dom); kids != 1 || text != "waiting for the town…" {
		t.Errorf("the unread panel drew %d lines (%q), want one", kids, text)
	}

	// A town whose seats read and whose queue did not says which half is
	// missing rather than drawing the queue as empty.
	dom = newPageDOM(t)
	dom.call(t, "renderWork", map[string]any{
		"summary": map[string]any{"seats_cap": 4},
		"polecats": []any{
			seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", time.Minute),
		},
	})
	if text := workText(t, dom); !strings.Contains(text, "reading the work queue…") {
		t.Errorf("a panel whose queue has not been read reads %q", text)
	}
}

// TestTheWorkRowsCannotOverflowTheirPanel pins the rule that keeps a seat's
// row inside its panel at every viewport from 1100 to 1920 px (gt-vcr3z). The
// table this replaced sized its columns off its longest cell and overflowed by
// 41 px at a 1350 px viewport. The unit tier has no browser, so the pin is the
// style itself: every column is a track that may shrink to nothing rather than
// a width a long bead title can push out, and a cell that runs out of room
// clips with an ellipsis instead of widening its neighbour.
func TestTheWorkRowsCannotOverflowTheirPanel(t *testing.T) {
	t.Parallel()

	row := cssRule(t, ".workrow")
	cols := ""
	for _, decl := range strings.Split(row, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(decl), "grid-template-columns:"); ok {
			cols = v
		}
	}
	if cols == "" {
		t.Fatal("the Work panel's rows lay out no columns")
	}
	tracks := strings.Fields(cols)
	if len(tracks) != 5 {
		t.Errorf("a seat's row lays out %d columns (%q), want five: agent, polecat, bead, state, age", len(tracks), cols)
	}
	flex := 0
	for _, track := range tracks {
		if !strings.HasPrefix(track, "minmax(0,") {
			t.Errorf("the column %q can be widened by its own content; a track must shrink to nothing instead", track)
			continue
		}
		if strings.HasSuffix(track, "1fr)") {
			flex++
		}
	}
	if flex != 1 {
		t.Errorf("the row gives %d columns what is left of the width, want exactly one (the bead and its title)", flex)
	}
	for _, want := range []string{"min-width:0", "overflow:hidden", "text-overflow:ellipsis", "white-space:nowrap"} {
		if !strings.Contains(cssRule(t, ".workrow>*"), want) {
			t.Errorf("a seat's cell carries no %q, so it can widen or overflow its panel", want)
		}
	}

	// The clipping above is only honest because the whole of a cell's text is
	// still readable: each cell carries it in its title attribute.
	if want := `const c = el("span", cls, text); if (tip) c.title = tip; tr.append(c); return c;`; !strings.Contains(pageFunc(t, "workRow"), want) {
		t.Errorf("a seat's cell does not carry its full text: %s", want)
	}
	if !strings.Contains(pageFunc(t, "workRow"), `bd.title = [p.bead, p.title].filter(Boolean).join(" ");`) {
		t.Error("a bead's cell does not carry the bead and its whole title, so a clipped title is lost")
	}
}

// TestTheWorkPanelIsTheOnlyPanelForSeatsAndQueue reads the embedded page: the
// Polecats and Work queue sections are gone, the Work section took their place
// in the left column, and no renderer draws them any more (gt-vcr3z).
func TestTheWorkPanelIsTheOnlyPanelForSeatsAndQueue(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	for _, gone := range []string{`id="seats"`, `id="queuenote"`, `id="seatsnote"`, "<h2>Polecats", "<h2>Work queue", "renderSeats", "renderQueue"} {
		if strings.Contains(page, gone) {
			t.Errorf("index.html still carries %q", gone)
		}
	}
	if !strings.Contains(page, `<section><h2>Work <span class="r" id="worknote"></span></h2><div class="body" id="work"></div></section>`) {
		t.Error("the left column has no Work section in the shape the page's panels take")
	}
	// The pane draws both readings it is joined from, and redraws on the tick
	// that moves its ages.
	if !strings.Contains(page, "workSeats(box, s, pcs, rSeats);") || !strings.Contains(page, "workQueue(box, s, q, rQueue);") {
		t.Error("the panel does not draw both the seats and the queue it replaced")
	}
}
