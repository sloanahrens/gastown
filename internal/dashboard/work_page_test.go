package dashboard

import (
	"encoding/json"
	"strconv"
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
// holding a bead, one idle — and reads the rows back: the polecat, the agent
// the session runs, the bead with its title, the state and the bead's age,
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
	if got := rows[0]; strings.Join(got, " ") != "Polecat Agent Bead State Age" {
		t.Errorf("the header row reads %v", got)
	}
	busy := rows[1]
	if len(busy) != 5 {
		t.Fatalf("a seat's row has %d cells (%v), want five", len(busy), busy)
	}
	if busy[0] != "dementus" {
		t.Errorf("the row does not lead with the polecat: %v", busy)
	}
	if busy[1] != "flash" {
		t.Errorf("the row does not name the agent its session runs: %v", busy)
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
	// hold something rather than left out. It holds no bead, so it names no
	// agent and names its rig in the Bead column instead (gt-hnztc).
	idle := rows[2]
	if len(idle) != 5 || idle[0] != "furiosa" || idle[1] != "" || idle[2] != "gastown" || idle[3] != "idle" || idle[4] != "" {
		t.Errorf("the idle seat's row reads %v", idle)
	}
	if !strings.Contains(workText(t, dom), "1 idle (free to take work; the model is chosen when work is dispatched)") {
		t.Error("the idle seat is not folded under a summary that says what it is")
	}
}

// TestTheWorkPanelGivesASeatHoldingNoBeadItsRig drives the panel over the
// seats an idle pool holds — one whose agent bead still records the model of
// the last job it ran, one with nothing recorded, and a seat in a parked rig
// holding nothing — beside two seats that do hold a bead, one of them in a
// parked rig. A seat holding nothing draws an empty Agent cell and names its
// rig where the bead would go, so the row says where the seat is rather than
// which model its last job ran; a seat holding a bead reads as it always did
// (gt-hnztc).
func TestTheWorkPanelGivesASeatHoldingNoBeadItsRig(t *testing.T) {
	t.Parallel()

	recorded := seat("devops", "capstan", "idle", "", "", "deepseek-flash", 0)
	blank := seat("devops", "halyard", "idle", "", "", "", 0)
	parkedIdle := seat("devops", "anchor", "idle", "", "", "claude-sonnet", 0)
	parkedIdle["rig_parked"] = true
	parkedBusy := seat("devops", "windlass", "working", "dv-parked", "work in a parked rig", "claude-sonnet", time.Minute)
	parkedBusy["rig_parked"] = true
	working := seat("gastown", "dementus", "working", "gt-hnztc", "an idle seat names its rig", "deepseek-flash", time.Minute)

	dom := newPageDOM(t)
	dom.call(t, "renderWork", workState([]any{working, recorded, blank, parkedIdle, parkedBusy}, workQueueReading(1, 0, 0, 0)))

	// The rows come back in the panel's own order: the seats holding a bead,
	// the parked fold (the working seat of it first), then the idle seats.
	agents := workCells(t, dom, "wagent")
	if len(agents) != 5 {
		t.Fatalf("the panel drew %d agent cells, want 5", len(agents))
	}
	if agents[0].Text != "flash" || agents[1].Text != "sonnet" {
		t.Errorf("a seat holding a bead no longer names the agent its session runs: %q, %q", agents[0].Text, agents[1].Text)
	}
	for i, who := range []string{"the parked rig's seat", "the idle seat with a recorded agent", "the idle seat with none"} {
		if got := agents[i+2]; got.Text != "" {
			t.Errorf("%s names a model: %q", who, got.Text)
		}
	}
	if want := "the model is chosen when work is dispatched"; !strings.Contains(agents[2].Title, want) {
		t.Errorf("an idle row does not say its model comes with the work: %q", agents[2].Title)
	}

	// The rig stands in the Bead column, where the bead would have been.
	beads := workCells(t, dom, "wbead")
	if len(beads) != 5 {
		t.Fatalf("the panel drew %d bead cells, want 5", len(beads))
	}
	if !strings.Contains(beads[1].Text, "dv-parked") || !strings.Contains(beads[1].Text, "work in a parked rig") {
		t.Errorf("a parked rig's seat holding a bead no longer carries it: %q", beads[1].Text)
	}
	for i := 2; i < 5; i++ {
		if beads[i].Text != "devops" {
			t.Errorf("a seat holding no bead does not name its rig: %q", beads[i].Text)
		}
	}
	if !strings.Contains(beads[3].Title, "devops") {
		t.Errorf("the rig the seat names does not carry its rig on hover: %q", beads[3].Title)
	}

	if text := workText(t, dom); !strings.Contains(text, "2 idle (free to take work; the model is chosen when work is dispatched)") {
		t.Errorf("the idle fold does not say when a seat's model is chosen: %q", text)
	}
}

// TestTheWorkPanelNamesAModelOnlyForABeadASeatHolds drives the panel over the
// rows the Agent cell is read from where the state and the bead disagree: a
// seat read as idle that does hold a bead names the model that bead was
// dispatched with, a seat holding nothing whose state carries a reason keeps
// that reason in the Bead column, and a parked rig's seat holding nothing
// names its rig with the inventory's reason beside it on hover. The reason a
// seat was read into is named wherever the row draws without a rig (gt-70txs).
func TestTheWorkPanelNamesAModelOnlyForABeadASeatHolds(t *testing.T) {
	t.Parallel()

	// A hook can outlive the read of the seat's state, so the two disagree.
	held := seat("gastown", "dementus", "idle", "gt-70txs", "the row names the bead's model", "deepseek-flash", time.Minute)
	// A seat holding nothing with no rig in its reading: the reason stands
	// where the rig would.
	reasoned := seat("", "flotsam", "recovery", "", "", "", 0)
	reasoned["reason"] = "its worktree needs recovery before reuse"
	parkedIdle := seat("devops", "anchor", "parked", "", "", "claude-sonnet", 0)
	parkedIdle["rig_parked"] = true
	parkedIdle["reason"] = "reusable"

	dom := newPageDOM(t)
	dom.call(t, "renderWork", workState([]any{held, reasoned, parkedIdle}, workQueueReading(0, 0, 0, 0)))

	// The seats holding nothing, then the seat holding a bead, in the panel's
	// own order: the busy fold, the parked fold, then the idle fold.
	agents := workCells(t, dom, "wagent")
	if len(agents) != 3 {
		t.Fatalf("the panel drew %d agent cells, want 3", len(agents))
	}
	if agents[0].Text != "" || agents[1].Text != "" {
		t.Errorf("a seat holding nothing names a model: %q, %q", agents[0].Text, agents[1].Text)
	}
	if agents[2].Text != "flash" {
		t.Errorf("a seat holding a bead does not name the model it was dispatched with: %q", agents[2].Text)
	}

	beads := workCells(t, dom, "wbead")
	if len(beads) != 3 {
		t.Fatalf("the panel drew %d bead cells, want 3", len(beads))
	}
	if beads[0].Text != "its worktree needs recovery before reuse" {
		t.Errorf("a seat with no rig drops the reason it was read into: %q", beads[0].Text)
	}
	if !strings.Contains(beads[1].Text, "devops") {
		t.Errorf("a parked rig's seat holding nothing does not name its rig: %q", beads[1].Text)
	}
	if !strings.Contains(beads[1].Title, "reusable") {
		t.Errorf("the reason the parked seat was read into is not on hover: %q", beads[1].Title)
	}
	if !strings.Contains(beads[2].Text, "gt-70txs") {
		t.Errorf("the seat holding a bead does not name it: %q", beads[2].Text)
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

// TestTheWorkPanelFoldsTheWholeQueueList reads the queue list back as one
// disclosure inside the panel, open on a fresh page load (gt-u9o36).
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
	if open.String() != "true" {
		t.Errorf("the queue list starts folded, want it open on the first render")
	}
	if !strings.Contains(list.Text, "the full queue list") {
		t.Errorf("the list's summary reads %q", list.Text)
	}
}

// queueListOpen reads whether the Work panel's queue list is drawn open.
func queueListOpen(t *testing.T, dom *pageDOM) string {
	t.Helper()

	v, err := dom.vm.RunString(`String(__byId["work"].children.find(c => String(c.className).indexOf("wqueue") >= 0).open)`)
	if err != nil {
		t.Fatalf("reading the list's fold: %v", err)
	}
	return v.String()
}

// TestTheWorkPanelKeepsTheQueueListClosedOnceAnOperatorClosesIt closes the
// list the way a click does and polls again: a poll that changed nothing
// leaves the element alone, and one that redraws the panel brings the list back
// closed, as the operator left it (gt-u9o36).
func TestTheWorkPanelKeepsTheQueueListClosedOnceAnOperatorClosesIt(t *testing.T) {
	t.Parallel()

	seats := func() []any {
		return []any{seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", time.Minute)}
	}
	st := workState(seats(), workQueueReading(57, 1, 2, 3))

	dom := newPageDOM(t)
	dom.call(t, "renderWork", st)
	if got := queueListOpen(t, dom); got != "true" {
		t.Fatalf("the list reads open = %s on the first render, want true", got)
	}

	// A click on the summary: the stub flips open and fires the page's handler.
	if _, err := dom.vm.RunString(`(() => {
		const d = __byId["work"].children.find(c => String(c.className).indexOf("wqueue") >= 0);
		globalThis.__queueList = d;
		__clickSummary(d);
	})()`); err != nil {
		t.Fatal(err)
	}
	if got := queueListOpen(t, dom); got != "false" {
		t.Fatalf("a click on the open list left it open = %s, want false", got)
	}

	dom.call(t, "renderWork", st)
	same, err := dom.vm.RunString(`String(__byId["work"].children.find(c => String(c.className).indexOf("wqueue") >= 0) === globalThis.__queueList)`)
	if err != nil {
		t.Fatal(err)
	}
	if same.String() != "true" {
		t.Error("an unchanged poll rebuilt the queue list")
	}

	next := workState(seats(), workQueueReading(58, 1, 2, 3))
	dom.call(t, "renderWork", next)
	redrawn, err := dom.vm.RunString(`String(__byId["work"].children.find(c => String(c.className).indexOf("wqueue") >= 0) !== globalThis.__queueList)`)
	if err != nil {
		t.Fatal(err)
	}
	if redrawn.String() != "true" {
		t.Fatal("a changed reading did not redraw the list, so the test says nothing about the next poll")
	}
	if got := queueListOpen(t, dom); got != "false" {
		t.Errorf("the list reads open = %s after the operator closed it and a poll redrew it, want false", got)
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
// unit tier has no browser, so the pin is the style itself: every column is a track that may shrink to nothing rather than
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
	// Every track is named for the cell it holds, so the template states the
	// column order workHead and workRow draw instead of describing an order
	// the page no longer uses (gt-dp6u3 swapped the first two).
	var tracks, names []string
	for _, tok := range strings.Fields(cols) {
		if n, ok := strings.CutPrefix(tok, "["); ok {
			names = append(names, strings.TrimSuffix(n, "]"))
			continue
		}
		tracks = append(tracks, tok)
	}
	if got, want := strings.Join(names, " "), "polecat agent bead state age"; got != want {
		t.Errorf("a seat's row names its columns %q, want %q in the order the header draws them", got, want)
	}
	if len(tracks) != 5 {
		t.Errorf("a seat's row lays out %d columns (%q), want five: polecat, agent, bead, state, age", len(tracks), cols)
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
	if !strings.Contains(pageFunc(t, "workRow"), `bd.title = [p.bead, p.title].filter(Boolean).join(" ") + (p.description ? "\n\n" + p.description : "");`) {
		t.Error("a bead's cell does not carry the bead and its whole title, so a clipped title is lost")
	}
}

// TestTheWorkPanelShortensTheAgentButKeepsItsFullNameOnHover drives the panel
// over seats whose agents carry each provider prefix and one that carries none,
// and reads the Agent cell back against the polecat in its row. A claude- or
// deepseek- prefix goes from the cell; any other name shows as it is; the
// tooltip keeps the whole name every time (gt-dp6u3).
func TestTheWorkPanelShortensTheAgentButKeepsItsFullNameOnHover(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderWork", workState([]any{
		seat("gastown", "alpha", "working", "gt-a1", "sandboxed", "deepseek-flash-sandbox", time.Minute),
		seat("gastown", "bravo", "working", "gt-a2", "haiku", "claude-haiku", time.Minute),
		seat("gastown", "charlie", "working", "gt-a3", "unprefixed", "gemini-pro", time.Minute),
	}, workQueueReading(0, 0, 0, 0)))

	names := workCells(t, dom, "wname")
	agents := workCells(t, dom, "wagent")
	if len(names) != 3 || len(agents) != 3 {
		t.Fatalf("the panel drew %d polecat cells and %d agent cells, want 3 of each", len(names), len(agents))
	}
	byPolecat := map[string]drawn{}
	for i, n := range names {
		byPolecat[n.Text] = agents[i]
	}
	for _, c := range []struct{ polecat, shown, full string }{
		{"alpha", "flash-sandbox", "deepseek-flash-sandbox"},
		{"bravo", "haiku", "claude-haiku"},
		{"charlie", "gemini-pro", "gemini-pro"},
	} {
		got, ok := byPolecat[c.polecat]
		if !ok {
			t.Errorf("the panel drew no row for %s", c.polecat)
			continue
		}
		if got.Text != c.shown {
			t.Errorf("%s's agent cell reads %q, want %q", c.polecat, got.Text, c.shown)
		}
		if !strings.Contains(got.Title, c.full) {
			t.Errorf("%s's agent cell does not carry the full name %q on hover: %q", c.polecat, c.full, got.Title)
		}
	}
}

// TestTheWorkQueueScrollsOnlyItsBeadRows pins the queue list's scroll region:
// the caption and filter chips stay in view, and only the bead rows scroll
// inside their own bounded region (gt-dp6u3).
func TestTheWorkQueueScrollsOnlyItsBeadRows(t *testing.T) {
	t.Parallel()

	rows := cssRule(t, ".qrows")
	if !strings.Contains(rows, "max-height:70vh") || !strings.Contains(rows, "overflow:auto") {
		t.Errorf("the queue's bead rows do not scroll in a bounded region: %q", rows)
	}
	if strings.Contains(string(indexHTML), ".qlist{") {
		t.Error("the queue list has a rule of its own, which would scroll its caption and chips with its rows")
	}

	dom := newPageDOM(t)
	dom.call(t, "renderWork", workState([]any{
		seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", time.Minute),
	}, workQueueReading(1, 0, 0, 0)))

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		let list = null, region = null;
		(function walk(n) {
			for (const c of (n.children || [])) {
				if (String(c.className).split(" ").indexOf("qlist") >= 0) list = c;
				walk(c);
			}
		})(__byId["work"]);
		const isRow = c => String(c.className).split(" ").indexOf("qrow") >= 0;
		region = list.children.find(c => String(c.className).split(" ").indexOf("qrows") >= 0);
		return {
			kids: list.children.map(c => String(c.className)),
			rowsDirect: list.children.filter(isRow).length,
			rowsInRegion: region ? region.children.filter(isRow).length : -1
		};
	})())`)
	if err != nil {
		t.Fatalf("reading the queue list's layout: %v", err)
	}
	var got struct {
		Kids         []string `json:"kids"`
		RowsDirect   int      `json:"rowsDirect"`
		RowsInRegion int      `json:"rowsInRegion"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the queue list's layout: %v", err)
	}
	if got.RowsDirect != 0 {
		t.Errorf("%d bead rows sit outside the region that scrolls", got.RowsDirect)
	}
	if got.RowsInRegion != 1 {
		t.Errorf("the scrolling region holds %d bead rows, want the one ready bead", got.RowsInRegion)
	}
	if n := len(got.Kids); n == 0 || got.Kids[n-1] != "qrows" {
		t.Fatalf("the queue list's last child is not the scrolling rows region: %v", got.Kids)
	}
	fixed := strings.Join(got.Kids[:len(got.Kids)-1], " ")
	// The caption wears its own class: the generic "empty" one also marks the
	// "nothing matches these filters" placeholder inside the scrolling rows, so
	// a test looking for it would pass on a queue list with no caption at all.
	for _, want := range []string{"qtabs", "qcaption"} {
		if !strings.Contains(fixed, want) {
			t.Errorf("the queue list draws no %s above its scrolling rows, so it scrolls out of view: %v", want, got.Kids)
		}
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

// workClock pins the page's clock to at, so a test moves time itself instead of
// reading the machine's.
func workClock(t *testing.T, dom *pageDOM, at time.Time) {
	t.Helper()

	if _, err := dom.vm.RunString(`Date.now = () => ` + strconv.FormatInt(at.UnixMilli(), 10)); err != nil {
		t.Fatalf("pinning the page's clock: %v", err)
	}
}

// workCells reads the text, class and title of every cell of class cls in the
// seat rows, in the order the panel drew them.
func workCells(t *testing.T, dom *pageDOM, cls string) []drawn {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const out = [];
		(function walk(n) {
			for (const c of (n.children || [])) {
				if (String(c.className).split(" ").indexOf("workrow") >= 0) {
					const x = c.children.find(k => String(k.className).split(" ").indexOf(` + quoteJS(cls) + `) >= 0);
					if (x) out.push({text: x.textContent, class: x.className, title: x.title});
				}
				walk(c);
			}
		})(__byId["work"]);
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the %s cells: %v", cls, err)
	}
	var out []drawn
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the %s cells: %v", cls, err)
	}
	return out
}

// TestTheWorkPanelMovesItsAgesBetweenPolls redraws an unchanged reading ten
// minutes later and reads the age cell back: the bead has been on the seat ten
// minutes longer, the cell wears the warning once its session has been silent
// for ten, and the row is the same element, so a poll that changed nothing
// does not rebuild the list an operator is reading.
func TestTheWorkPanelMovesItsAgesBetweenPolls(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	p := seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", 12*time.Minute)
	at := base.Add(-time.Minute).Format(time.RFC3339)
	p["slung"], p["last_active"] = base.Add(-12*time.Minute).Format(time.RFC3339), at
	st := workState([]any{p}, workQueueReading(57, 1, 3, 0))
	st["summary_at"] = base.Format(time.RFC3339)
	for _, r := range st["reads"].(map[string]any) {
		r.(map[string]any)["at"] = base.Format(time.RFC3339)
	}

	dom := newPageDOM(t)
	workClock(t, dom, base)
	dom.call(t, "renderWork", st)
	if _, err := dom.vm.RunString(`globalThis.__seatRow = __byId["work"].children[1]`); err != nil {
		t.Fatal(err)
	}
	if got := workCells(t, dom, "wnum"); len(got) != 1 || got[0].Text != "12m" || strings.Contains(got[0].Class, "warnc") {
		t.Fatalf("the age cell reads %+v before the clock moves, want 12m and no warning", got)
	}

	workClock(t, dom, base.Add(10*time.Minute))
	dom.call(t, "renderWork", st)
	got := workCells(t, dom, "wnum")
	if len(got) != 1 || got[0].Text != "22m" {
		t.Errorf("the age cell reads %+v ten minutes on, want 22m", got)
	}
	if !strings.Contains(got[0].Class, "warnc") || !strings.Contains(got[0].Title, "last printed 11m ago") {
		t.Errorf("the age cell reads %+v: want the warning and the silence in its title", got[0])
	}
	same, err := dom.vm.RunString(`String(__byId["work"].children[1] === globalThis.__seatRow)`)
	if err != nil {
		t.Fatal(err)
	}
	if same.String() != "true" {
		t.Error("an unchanged reading rebuilt the seat's row instead of moving its age in place")
	}
}

// TestTheWorkPanelNamesTheLandingsAndOrdersTheIdleByThem reads the polecat's
// cell of a seat that landed beads: its title carries the 24-hour landings the
// old table drew as columns, and the idle seats are folded most-landed first.
func TestTheWorkPanelNamesTheLandingsAndOrdersTheIdleByThem(t *testing.T) {
	t.Parallel()

	busy := seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", time.Minute)
	busy["landed_24h"], busy["approved_24h"], busy["avg_score_24h"] = 4, 3, 0.8
	quiet := seat("gastown", "aaa", "idle", "", "", "", 0)
	busyIdle := seat("gastown", "zzz", "idle", "", "", "", 0)
	busyIdle["landed_24h"], busyIdle["approved_24h"] = 2, 2

	dom := newPageDOM(t)
	dom.call(t, "renderWork", workState([]any{busy, quiet, busyIdle}, workQueueReading(1, 0, 0, 0)))

	names := workCells(t, dom, "wname")
	if len(names) != 3 {
		t.Fatalf("the panel drew %d polecat cells, want 3", len(names))
	}
	if want := "gastown/dementus · landed 4 in 24h, om approved 75%, average score 0.80"; names[0].Title != want {
		t.Errorf("the busy seat's polecat title reads %q, want %q", names[0].Title, want)
	}
	if names[1].Text != "zzz" || names[2].Text != "aaa" {
		t.Errorf("the idle seats are folded %q then %q, want the one that landed first", names[1].Text, names[2].Text)
	}
	if names[2].Title != "gastown/aaa" {
		t.Errorf("a seat with no landings carries %q in its title, want only its rig/name", names[2].Title)
	}
}

// TestTheWorkPanelBlockedLineIsNeverCleanOverAnIncompleteReading drives the
// blocked line over the readings that cannot support a count: a queue read that
// failed, a server that sent no split, and a store that could not be read. Each
// says so in the warning colour rather than drawing "0 beads blocked".
func TestTheWorkPanelBlockedLineIsNeverCleanOverAnIncompleteReading(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Format(time.RFC3339)
	failed := func(s map[string]any) {
		s["reads"].(map[string]any)["queue"] = map[string]any{"at": now, "every_sec": 60, "error": true, "stale": true}
	}
	tests := []struct {
		name    string
		mutate  func(s map[string]any)
		want    []string
		notWant []string
	}{
		{"the queue read failed", failed, []string{"blocked: unavailable", "the queue read failed"}, []string{"beads blocked"}},
		{"no split was sent", func(s map[string]any) {
			delete(s["queue"].(map[string]any), "blocked_work")
			delete(s["queue"].(map[string]any), "blocked_wisps")
		}, []string{"blocked: unavailable", "no blocked/wisp split"}, []string{"beads blocked", "0"}},
		{"a store could not be read", func(s map[string]any) {
			s["queue"].(map[string]any)["unreadable"] = []any{"beads"}
		}, []string{"at least 3 beads blocked", "beads unreadable"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st := workState([]any{seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", time.Minute)}, workQueueReading(57, 1, 3, 0))
			tt.mutate(st)
			dom := newPageDOM(t)
			dom.call(t, "renderWork", st)
			line := workChild(t, dom, "wblocked")
			if !strings.Contains(line.Class, "warnc") {
				t.Errorf("the blocked line wears %q, want the warning class", line.Class)
			}
			for _, w := range tt.want {
				if !strings.Contains(line.Text, w) {
					t.Errorf("the blocked line reads %q, want %q in it", line.Text, w)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(line.Text, w) {
					t.Errorf("the blocked line reads %q, want no %q in it", line.Text, w)
				}
			}
		})
	}
}

// TestTheWorkPanelWearsTheWorseOfItsTwoReads drives the panel over each
// pairing of a fresh, an aged and a failed read of the seats and of the queue,
// and reads back the note's words and colour and whether the panel is greyed:
// a panel joined from two sources is only as current as the worse of them.
func TestTheWorkPanelWearsTheWorseOfItsTwoReads(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Format(time.RFC3339)
	fresh := map[string]any{"at": now, "every_sec": 60}
	aged := map[string]any{"at": now, "every_sec": 60, "stale": true}
	failed := map[string]any{"at": now, "every_sec": 60, "stale": true, "error": true}
	tests := []struct {
		name         string
		seats, queue map[string]any
		wantClass    string
		wantNote     []string
		greyed       bool
	}{
		{"both fresh", fresh, fresh, "r", nil, false},
		{"seats aged", aged, fresh, "r warnc", []string{"the seats read is old"}, true},
		{"queue failed", fresh, failed, "r badc", []string{"the queue read failed"}, true},
		{"seats aged and queue failed", aged, failed, "r badc", []string{"the seats read is old", "the queue read failed"}, true},
		{"seats failed and queue aged", failed, aged, "r badc", []string{"the seats read failed", "the queue read is old"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st := workState([]any{seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel", "deepseek-flash", time.Minute)}, workQueueReading(57, 1, 3, 0))
			st["reads"] = map[string]any{"seats": tt.seats, "queue": tt.queue}
			dom := newPageDOM(t)
			dom.call(t, "renderWork", st)
			note := dom.element(t, "worknote")
			if note.Class != tt.wantClass {
				t.Errorf("the note wears %q, want %q", note.Class, tt.wantClass)
			}
			for _, w := range tt.wantNote {
				if !strings.Contains(note.Text, w) {
					t.Errorf("the note reads %q, want %q in it", note.Text, w)
				}
			}
			if got := strings.Contains(dom.element(t, "work").Class, "stale"); got != tt.greyed {
				t.Errorf("the panel greyed = %v, want %v", got, tt.greyed)
			}
		})
	}
}

// TestTheWorkPanelSaysWhyItHasNoSeats drives the panel with no polecats: a
// seats read that failed says unavailable, and one that read fine says there
// are none.
func TestTheWorkPanelSaysWhyItHasNoSeats(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Format(time.RFC3339)
	for name, tc := range map[string]struct {
		seats map[string]any
		want  string
	}{
		"failed": {map[string]any{"at": now, "every_sec": 60, "stale": true, "error": true}, "unavailable — the seats read failed or went stale"},
		"fresh":  {map[string]any{"at": now, "every_sec": 60}, "no polecats"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			st := workState([]any{}, workQueueReading(1, 0, 0, 0))
			st["reads"] = map[string]any{"seats": tc.seats, "queue": map[string]any{"at": now, "every_sec": 60}}
			dom := newPageDOM(t)
			dom.call(t, "renderWork", st)
			if text := workText(t, dom); !strings.Contains(text, tc.want) {
				t.Errorf("a panel with no seats reads %q, want %q in it", text, tc.want)
			}
		})
	}
}

// beadTips reads the hover of every seat row's bead cell, in the order the
// rows are drawn, folded rows included.
func beadTips(t *testing.T, dom *pageDOM) []string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const out = [];
		(function walk(n) {
			for (const c of (n.children || [])) {
				if (String(c.className).split(" ").indexOf("workrow") >= 0) {
					out.push(c.children.filter(x => String(x.className).split(" ").indexOf("wbead") >= 0).map(x => x.title)[0] || "");
				}
				walk(c);
			}
		})(__byId["work"]);
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the seat rows' bead hovers: %v", err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the seat rows' bead hovers: %v", err)
	}
	return out
}

// TestTheWorkPanelShowsTheBeadsDescriptionOnHover drives the panel over a seat
// whose bead has a description and one whose bead has none: the first's bead
// cell hovers as its id, title and description, the second's as its id and
// title alone, and the row's text and column count do not change (gt-kk6gb).
func TestTheWorkPanelShowsTheBeadsDescriptionOnHover(t *testing.T) {
	t.Parallel()

	described := seat("gastown", "dementus", "working", "gt-kk6gb", "hover shows more", "deepseek-flash", time.Minute)
	described["description"] = "After this lands, hovering the bead shows its description."
	bare := seat("gastown", "furiosa", "stalled", "gt-bare", "no description here", "", 0)

	dom := newPageDOM(t)
	dom.call(t, "renderWork", workState([]any{described, bare}, workQueueReading(1, 0, 0, 0)))

	tips := beadTips(t, dom)
	if len(tips) != 3 {
		t.Fatalf("the panel drew %d rows (%v), want a header and two seats", len(tips), tips)
	}
	if want := "gt-kk6gb hover shows more\n\nAfter this lands, hovering the bead shows its description."; !contains(tips, want) {
		t.Errorf("the bead cells hover as %q, want one to read %q", tips, want)
	}
	if !contains(tips, "gt-bare no description here") {
		t.Errorf("a bead with no description must hover as its title alone, got %q", tips)
	}
	for _, row := range workRows(t, dom)[1:] {
		if len(row) != 5 {
			t.Errorf("a seat's row has %d cells (%v), want the same five as before", len(row), row)
		}
		if strings.Contains(strings.Join(row, " "), "After this lands") {
			t.Errorf("the description must stay in the hover, not the row's text: %v", row)
		}
	}
}

// TestTheWorkPanelRedrawsWhenABeadsDescriptionChanges: the panel is left alone
// while its key is unchanged, so an edited description must change the key or
// the hover would keep the old text until something else moved (gt-kk6gb).
func TestTheWorkPanelRedrawsWhenABeadsDescriptionChanges(t *testing.T) {
	t.Parallel()

	one := seat("gastown", "dementus", "working", "gt-kk6gb", "hover shows more", "deepseek-flash", time.Minute)
	one["description"] = "first"
	dom := newPageDOM(t)
	s := workState([]any{one}, workQueueReading(1, 0, 0, 0))
	dom.call(t, "renderWork", s)

	one["description"] = "second"
	dom.call(t, "renderWork", s)
	if tips := beadTips(t, dom); len(tips) != 2 || !strings.HasSuffix(tips[1], "second") {
		t.Errorf("after the description changed the bead cell hovers as %q, want it to end with the new text", tips)
	}
}

// TestTheWorkPanelRedrawsWhenACommaMovesBetweenTitleAndDescription: the panel's
// key must tell apart two seats whose fields join to the same text, or a
// description edit that only moves a comma would keep the old hover (gt-noda1).
func TestTheWorkPanelRedrawsWhenACommaMovesBetweenTitleAndDescription(t *testing.T) {
	t.Parallel()

	one := seat("gastown", "dementus", "working", "gt-noda1", "a,b", "deepseek-flash", time.Minute)
	one["description"] = "c"
	dom := newPageDOM(t)
	s := workState([]any{one}, workQueueReading(1, 0, 0, 0))
	dom.call(t, "renderWork", s)

	one["title"] = "a"
	one["description"] = "b,c"
	dom.call(t, "renderWork", s)
	if tips := beadTips(t, dom); len(tips) != 2 || tips[1] != "gt-noda1 a\n\nb,c" {
		t.Errorf("after a comma moved from the title to the description the bead cell hovers as %q, want gt-noda1 a, a blank line and b,c", tips)
	}
}

// TestTheWorkPanelRedrawsWhenASeatsRigIsParked: a park moves a seat holding
// work into the parked fold while the disposition's reason stands — nothing
// else in the reading changes — so the panel's key carries rig_parked on its
// own, or the row stays in the fold it was drawn in until something else moves
// (gt-u53jg).
func TestTheWorkPanelRedrawsWhenASeatsRigIsParked(t *testing.T) {
	t.Parallel()

	held := seat("gastown", "dementus", "working", "gt-u53jg", "a park its reason does not mirror", "deepseek-flash", time.Minute)
	dom := newPageDOM(t)
	s := workState([]any{held}, workQueueReading(1, 0, 0, 0))
	dom.call(t, "renderWork", s)
	fold := "1 in parked rigs (stood down on purpose; not alerts)"
	if text := workText(t, dom); strings.Contains(text, fold) {
		t.Fatalf("the seat is folded as parked before its rig was parked: %q", text)
	}

	held["rig_parked"] = true
	dom.call(t, "renderWork", s)
	if text := workText(t, dom); !strings.Contains(text, fold) {
		t.Errorf("after only the rig was parked the panel still draws %q, want the seat under %q", text, fold)
	}

	delete(held, "rig_parked")
	dom.call(t, "renderWork", s)
	if text := workText(t, dom); strings.Contains(text, fold) {
		t.Errorf("after only the rig was unparked the panel still draws %q, want the seat back with the seats holding work", text)
	}
}
