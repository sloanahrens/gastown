package dashboard

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// The panel's rows and its note are drawn by index.html's own script, so the
// tests below run that script over the reading the server would send
// (page_harness_test.go) rather than matching its source: a render function
// that mentions the right names draws nothing (gt-tq4ee).

// attnRowsJS is the panel's rows as #attention holds them, in the panel's
// order, without the group headings the panel draws between them: the headings
// are the panel's, and a test that counted them as rows would read the two
// groups as two more items (gt-tv5v2).
const attnRowsJS = `Array.from(__byId["attention"].children).filter(c => !c.classList.contains("agroup"))`

// panelDrawn is one row of the panel as the page drew it: the text the operator
// reads, the owner tag the row carries, the classes of its cells and whether
// the owner filter has hidden it.
type panelDrawn struct {
	Text   string   `json:"text"`
	Owner  string   `json:"owner"`
	Hidden bool     `json:"hidden"`
	Cells  []string `json:"cells"`
}

// panelRows reads the rows of #attention, in the order the panel drew them.
// __snap reads one element by id and the filter sets a flag on the rows inside
// it, so the children are read where the drawing put them.
func panelRows(t *testing.T, dom *pageDOM) []panelDrawn {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify(` + attnRowsJS + `.map(c => ({
		text: c.textContent, owner: c.dataset.owner || "", hidden: !!c.hidden,
		cells: (function walk(e) {
			return Array.from(e.children).reduce((out, k) =>
				out.concat([k.tagName.toLowerCase() + " " + k.className], walk(k)), []);
		})(c)})))`)
	if err != nil {
		t.Fatalf("reading the panel's rows: %v", err)
	}
	var out []panelDrawn
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the panel's rows: %v", err)
	}
	return out
}

// panelCells reports whether one row drew a cell of the given tag and class,
// anywhere under it: a row's header cells are inside the button that folds it.
func panelCells(row panelDrawn, cell string) bool {
	for _, c := range row.Cells {
		if c == cell {
			return true
		}
	}
	return false
}

// switchOwner switches one owner's chip, the way the operator does. The chips
// are drawn in OWNERS order and each names its owner in its title, which is how
// the test finds it.
func switchOwner(t *testing.T, dom *pageDOM, owner string) {
	t.Helper()

	js := `(() => {
		const chips = Array.from(__byId["attentionchips"].children);
		const i = chips.findIndex(b => b.title.indexOf("` + owner + `") >= 0);
		if (i < 0) throw new Error("no chip names ` + owner + `");
		chips[i].onclick();
	})()`
	if _, err := dom.vm.RunString(js); err != nil {
		t.Fatalf("switching the %s chip: %v", owner, err)
	}
}

// attentionState is the reading the panel is drawn from: the rows of the one
// list, and what the readers behind it had to say.
func attentionState(at map[string]any) map[string]any {
	return map[string]any{"attention": at}
}

// attentionFoldRows is the reading the fold tests draw: one row of each kind,
// the question carrying the long body a fold is for and each of the others the
// cells its kind adds. Every call is the same reading, which is what a redraw
// with nothing new looks like.
func attentionFoldRows() []any {
	return []any{
		map[string]any{"kind": "question", "owner": "operator", "id": "gt-q1", "title": "retry policy",
			"priority": 1, "question": "how many times should a push retry?",
			"recommended": "retry twice\nand page someone after an hour", "effort": "quick",
			"held_count": 2, "held": []any{"gt-h1"}, "state": "awaiting", "age_sec": 5400},
		map[string]any{"kind": "escalation", "owner": "overseer", "id": "hq-9", "title": "slot held",
			"severity": "critical", "escalated_by": "gastown/polecats/opal", "age_sec": 600},
		map[string]any{"kind": "item", "owner": "overseer", "id": "slot:gastown", "title": "the slot is held",
			"level": "crit", "rule": "slot.held", "age_sec": 60},
	}
}

// panelRow is one row of the panel read back as an element: the tag the row
// itself is, the tag of the button its header line folds from, the
// aria-expanded that button carries and the text the row reads.
type panelRow struct {
	Tag      string `json:"tag"`
	Head     string `json:"head"`
	Expanded string `json:"expanded"`
	Text     string `json:"text"`
}

// readRow reads one row of #attention as the page drew it. A row the panel did
// not draw is a fatal: the test would otherwise read zero values as a shape.
func readRow(t *testing.T, dom *pageDOM, i int) panelRow {
	t.Helper()

	js := `JSON.stringify((() => {
		const r = (` + attnRowsJS + `)[` + strconv.Itoa(i) + `];
		if (!r) return null;
		const head = Array.from(r.children).find(c => c.className === "arow-head");
		return {tag: r.tagName.toLowerCase(), head: head ? head.tagName.toLowerCase() : "",
			expanded: head ? head.getAttribute("aria-expanded") : null, text: r.textContent};
	})())`
	v, err := dom.vm.RunString(js)
	if err != nil {
		t.Fatalf("reading row %d: %v", i, err)
	}
	if v.String() == "null" {
		t.Fatalf("the panel drew no row %d", i)
	}
	var out panelRow
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading row %d: %v", i, err)
	}
	return out
}

// clickRow clicks one of the panel's rows, the way the operator does: on the
// header line, which is the button the row folds from.
func clickRow(t *testing.T, dom *pageDOM, i int) {
	t.Helper()

	js := `(() => {
		const r = (` + attnRowsJS + `)[` + strconv.Itoa(i) + `];
		if (!r) throw new Error("the panel drew no row ` + strconv.Itoa(i) + `");
		const head = Array.from(r.children).find(c => c.className === "arow-head");
		if (!head) throw new Error("row ` + strconv.Itoa(i) + ` drew no header line to click");
		__click(head);
	})()`
	if _, err := dom.vm.RunString(js); err != nil {
		t.Fatalf("clicking row %d: %v", i, err)
	}
}

// rowFolds reads the aria-expanded of every row the panel drew, in the order it
// drew them: a row is open or folded on its own, and this is the whole set.
func rowFolds(t *testing.T, dom *pageDOM) []string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((` + attnRowsJS + `).map(r => {
		const head = Array.from(r.children).find(c => c.className === "arow-head");
		return head ? head.getAttribute("aria-expanded") : null;
	}))`)
	if err != nil {
		t.Fatalf("reading the rows' folds: %v", err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the rows' folds: %v", err)
	}
	return out
}

// storedFolds reads the folds the page has kept, keyed the way it keys them:
// the item's name and the hash of what its row says.
func storedFolds(t *testing.T, dom *pageDOM) map[string]bool {
	t.Helper()

	var out map[string]bool
	if err := json.Unmarshal([]byte(storedFoldsJSON(t, dom)), &out); err != nil {
		t.Fatalf("reading the page's folds: %v", err)
	}
	return out
}

// storedFoldsJSON reads the folds the page has kept as the store holds them.
func storedFoldsJSON(t *testing.T, dom *pageDOM) string {
	t.Helper()

	v, err := dom.vm.RunString(`localStorage.getItem("gt-attn-collapsed") || "{}"`)
	if err != nil {
		t.Fatalf("reading the page's folds: %v", err)
	}
	return v.String()
}

// handStore hands one page everything another has written, the way the browser
// gives a tab what another tab stored.
func handStore(t *testing.T, from, to *pageDOM) {
	t.Helper()

	for k, v := range from.storage(t) {
		if _, err := to.vm.RunString("localStorage.setItem(" + strconv.Quote(k) + ", " + strconv.Quote(v) + ")"); err != nil {
			t.Fatalf("handing the second tab %s: %v", k, err)
		}
	}
}

// foldOfItem reports whether the page holds a fold for one item, whatever words
// its row was folded against.
func foldOfItem(t *testing.T, dom *pageDOM, item string) bool {
	t.Helper()

	for k := range storedFolds(t, dom) {
		if strings.HasPrefix(k, item+":") {
			return true
		}
	}
	return false
}

// chipReads reads the owner chips the panel drew, each as the count it carries
// and the owner it names.
func chipReads(t *testing.T, dom *pageDOM) []string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify(Array.from(__byId["attentionchips"].children).map(b =>
		Array.from(b.children).map(k => k.textContent).join("") + " " + b.title))`)
	if err != nil {
		t.Fatalf("reading the owner chips: %v", err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the owner chips: %v", err)
	}
	return out
}

// The panel opens on the human's rows — the criterion's 'Needs you' list — and
// the owner chips are its one filter: switching the overseer's chip draws its
// rows into the same list rather than into a panel of their own (gt-bi6w0).
func TestThePanelOpensOnTheHumanRowsAndTheChipsAddTheRest(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{
		map[string]any{"kind": "question", "owner": "operator", "id": "gt-q1", "title": "retry policy"},
		map[string]any{"kind": "escalation", "owner": "overseer", "id": "hq-9", "title": "slot held"},
		map[string]any{"kind": "item", "owner": "overseer", "id": "slot:gastown", "title": "the slot is held"},
	}}))

	rows := panelRows(t, dom)
	if len(rows) != 1 || rows[0].Owner != "operator" {
		t.Fatalf("the panel drew %+v before a chip was touched, want the human's row alone", rows)
	}

	// Each chip carries the count of the rows its owner holds, drawn from the
	// whole list, so the reader knows what the filter is holding back.
	v, err := dom.vm.RunString(`JSON.stringify(Array.from(__byId["attentionchips"].children).map(b => ({
		title: b.title, count: Array.from(b.children).map(k => k.textContent).join("")})))`)
	if err != nil {
		t.Fatalf("reading the owner chips: %v", err)
	}
	var chips []struct {
		Title string `json:"title"`
		Count string `json:"count"`
	}
	if err := json.Unmarshal([]byte(v.String()), &chips); err != nil {
		t.Fatalf("reading the owner chips: %v", err)
	}
	if len(chips) != 2 || !strings.Contains(chips[0].Title, "operator") || chips[0].Count != "1" ||
		!strings.Contains(chips[1].Title, "overseer") || chips[1].Count != "2" {
		t.Errorf("the chips read %+v, want the human's one row and the overseer's two", chips)
	}

	switchOwner(t, dom, "overseer")
	if rows := panelRows(t, dom); len(rows) != 3 {
		t.Errorf("the panel drew %+v with the overseer's chip on, want the whole list", rows)
	}
	// The note counts what the panel is showing, so the chip redraws it too
	// rather than leaving it counting the rows the filter held back.
	if note := dom.element(t, "attentionnote").Text; note != "3 need you · 0 in the pipeline" {
		t.Errorf("the note reads %q with the whole list shown, want it counting the three rows", note)
	}

	// The human's chip switched off leaves the overseer's rows.
	switchOwner(t, dom, "operator")
	rows = panelRows(t, dom)
	if len(rows) != 2 {
		t.Fatalf("the panel drew %+v with the human's chip off, want the overseer's two", rows)
	}
	for _, r := range rows {
		if r.Owner != "overseer" {
			t.Errorf("row %+v is still drawn with the human's chip off", r)
		}
	}

	// Every chip off is the reader's own doing, and the panel says so rather
	// than passing it off as a town with nothing waiting.
	switchOwner(t, dom, "overseer")
	if text := dom.element(t, "attention").Text; !strings.Contains(text, "every owner chip is off") {
		t.Errorf("the panel reads %q with every chip off", text)
	}
}

// A row whose owner the page has no chip for is drawn rather than hidden: a
// rule a newer daemon writes reaches the reader who can act on it instead of a
// filter nobody can switch (attention.ownerRank).
func TestThePanelDrawsARowNoChipOwns(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{
		map[string]any{"kind": "item", "owner": "someone", "id": "x:1", "title": "a rule from a newer daemon"},
	}}))
	if rows := panelRows(t, dom); len(rows) != 1 || rows[0].Owner != "someone" {
		t.Errorf("the panel drew %+v, want the row whose owner has no chip", rows)
	}
}

// One row per thing waiting, each tagged with its owner and carrying what
// answering it takes: the question's own words and the line that answers it,
// the escalation's severity and raiser, the queue item's rule and level
// (gt-bi6w0).
func TestThePanelDrawsTheThreeKindsOfRow(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{
		map[string]any{"kind": "question", "owner": "operator", "id": "gt-q1", "title": "retry policy",
			"question": "how many times should a push retry?", "recommended": "retry twice\nand page someone if it fails again",
			"held_count": 2, "held": []any{"gt-h1"}, "effort": "quick",
			"state": "awaiting", "priority": 0, "age_sec": 5400},
		map[string]any{"kind": "escalation", "owner": "overseer", "id": "hq-9", "title": "slot held",
			"severity": "critical", "escalated_by": "gastown/polecats/opal", "age_sec": 600},
		map[string]any{"kind": "item", "owner": "overseer", "id": "slot:gastown", "title": "the slot is held",
			"level": "crit", "rule": "slot.held", "age_sec": 60},
	}}))
	switchOwner(t, dom, "overseer") // the panel opens on the human's row alone

	rows := panelRows(t, dom)
	if len(rows) != 3 {
		t.Fatalf("the panel drew %d rows (%+v), want one per kind", len(rows), rows)
	}

	// The question leads: it is the row type that stops the town while it waits,
	// and it is the only one the human has to answer.
	q := rows[0]
	if q.Owner != "operator" || !panelCells(q, "span tag own-operator") {
		t.Errorf("the question row is tagged %q with the cells %v, want it the human's", q.Owner, q.Cells)
	}
	for _, want := range []string{
		"gt-q1", "retry policy", "how many times should a push retry?",
		"Recommended: retry twice", "quick", "awaiting you", "P0", "1h30m",
		"holds 2 beads", "Held: gt-h1", `gt answer gt-q1 "<answer>"`,
	} {
		if !strings.Contains(q.Text, want) {
			t.Errorf("the question row reads %q, with no %q", q.Text, want)
		}
	}
	// The recommended answer is a paragraph; the row carries its first line.
	if strings.Contains(q.Text, "and page someone") {
		t.Errorf("the question row reads the whole recommended answer: %q", q.Text)
	}
	// P0 is drawn in the amber the page marks what wants attention with, and
	// the line that answers the question is one the operator can select whole.
	if !panelCells(q, "span qpri warnc") {
		t.Errorf("the P0 question wears the cells %v, want the priority marked", q.Cells)
	}

	e := rows[1]
	if e.Owner != "overseer" {
		t.Errorf("the escalation row is tagged %q, want the overseer", e.Owner)
	}
	for _, want := range []string{"critical", "hq-9", "raised by gastown/polecats/opal", "slot held", "10m"} {
		if !strings.Contains(e.Text, want) {
			t.Errorf("the escalation row reads %q, with no %q", e.Text, want)
		}
	}

	i := rows[2]
	for _, want := range []string{"crit", "slot.held", "the slot is held", "60s"} {
		if !strings.Contains(i.Text, want) {
			t.Errorf("the queue item row reads %q, with no %q", i.Text, want)
		}
	}
}

// A queue item draws Jev's needs_human answer beside it, and an item no answer
// names draws a dash rather than a made-up zero: the reader sees the number the
// row is ranked by, and an unanswered item still stands in the list.
func TestThePanelDrawsTheNeedsHumanScore(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{
		map[string]any{"kind": "item", "owner": "overseer", "id": "stall:gastown/opal", "title": "opal is stalled",
			"level": "warn", "rule": "polecat.stalled", "age_sec": 60, "needs_human": 0.87},
		map[string]any{"kind": "item", "owner": "overseer", "id": "red-main:gastown", "title": "main is red",
			"level": "crit", "rule": "red-main", "age_sec": 30},
	}}))
	switchOwner(t, dom, "overseer")

	rows := panelRows(t, dom)
	if len(rows) != 2 {
		t.Fatalf("the panel drew %d rows (%+v), want both queue items", len(rows), rows)
	}
	if !strings.Contains(rows[0].Text, "needs 0.87") || !panelCells(rows[0], "span nh") {
		t.Errorf("the scored row reads %q with cells %v, want its 0.87 drawn", rows[0].Text, rows[0].Cells)
	}
	if !strings.Contains(rows[1].Text, "needs –") {
		t.Errorf("the unanswered row reads %q, want a dash rather than a score", rows[1].Text)
	}

	// An answers log nobody could read is not a dash: the panel says the read
	// failed and the rows read "?" instead, so the two states stay apart
	// (gt-b9j3o.2). The owner chip is already on the overseer's rows.
	dom.call(t, "renderAttention", attentionState(map[string]any{
		"scores_failed": true,
		"rows": []any{
			map[string]any{"kind": "item", "owner": "overseer", "id": "red-main:gastown", "title": "main is red",
				"level": "crit", "rule": "red-main", "age_sec": 30},
		}}))
	rows = panelRows(t, dom)
	var item *panelDrawn
	for i := range rows {
		if rows[i].Owner == "overseer" {
			item = &rows[i]
		}
	}
	if item == nil {
		t.Fatalf("the panel drew %+v, want the one item", rows)
	}
	if !strings.Contains(item.Text, "needs ?") {
		t.Errorf("a row of an unreadable answers log reads %q, want needs ?", item.Text)
	}
	if body := dom.element(t, "attention").Text; !strings.Contains(body, "needs_human answers could not be read") {
		t.Errorf("the panel does not say the answers log could not be read: %q", body)
	}
}

// The panel draws the rows in the two groups the reader acts on: what needs a
// person, then what is only waiting on work already in flight, with each
// group's count in the panel's header. A pipeline row names who holds it and
// what it waits on, on its one line, so work in flight never reads as a
// decision nobody made (gt-tv5v2).
func TestThePanelGroupsNeedsYouAndInThePipeline(t *testing.T) {
	t.Parallel()

	act := map[string]any{"kind": "item", "owner": "operator", "class": "act", "id": "host:disk",
		"title": "the volume is 96% full", "level": "crit", "rule": "host.disk_crit", "age_sec": 60}
	held := map[string]any{"kind": "item", "owner": "overseer", "class": "pipeline", "id": "rejected-twice:gt-9",
		"title": "gt-9 was rejected twice", "level": "warn", "rule": "landing.rejected_twice",
		"held_by": "opal", "waiting_on": "rework by opal, attempt 2", "age_sec": 120}
	ownerOnly := map[string]any{"kind": "item", "owner": "overseer", "class": "pipeline", "id": "risk:gt-7:deadbeef",
		"title": "gt-7 landed touching internal/cmd", "level": "warn", "rule": "landing.risk_path",
		"held_by": "opal", "age_sec": 180}

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{act, held, ownerOnly}}))
	switchOwner(t, dom, "overseer") // the panel opens on the human's rows; the overseer's are one click away

	if note := dom.element(t, "attentionnote").Text; note != "1 need you · 2 in the pipeline" {
		t.Errorf("the header reads %q, want the two groups' counts", note)
	}

	// The groups are drawn in the order the reader acts on them, each under its
	// heading, and every row sits under its own group's heading.
	v, err := dom.vm.RunString(`JSON.stringify(Array.from(__byId["attention"].children).map(c => ({
		group: c.classList.contains("agroup"), text: c.textContent})))`)
	if err != nil {
		t.Fatalf("reading the panel's groups: %v", err)
	}
	var drawn []struct {
		Group bool   `json:"group"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(v.String()), &drawn); err != nil {
		t.Fatalf("reading the panel's groups: %v", err)
	}
	var order []string
	for _, d := range drawn {
		if d.Group {
			order = append(order, d.Text)
		}
	}
	if len(order) != 2 || order[0] != "Needs you" || order[1] != "In the pipeline" {
		t.Fatalf("the panel draws the headings %v, want Needs you then In the pipeline", order)
	}
	rows := panelRows(t, dom)
	if len(rows) != 3 || !strings.Contains(rows[0].Text, "the volume is 96% full") {
		t.Fatalf("the panel drew %+v, want the act row first and the two pipeline rows behind it", rows)
	}
	found := map[string]panelDrawn{}
	for _, r := range rows[1:] {
		switch {
		case strings.Contains(r.Text, "gt-9 was rejected twice"):
			found["held"] = r
		case strings.Contains(r.Text, "gt-7 landed touching internal/cmd"):
			found["owner"] = r
		}
	}
	if len(found) != 2 {
		t.Fatalf("the panel drew %+v, want the two pipeline rows in its pipeline group", rows)
	}

	// One line per pipeline row, naming who holds it and what it waits on. A
	// row with no waiting-on line shows its owner alone, and neither has a
	// folded body to open: an item has nothing under its header line.
	for _, want := range []string{"held by opal", "waiting on rework by opal, attempt 2"} {
		if !strings.Contains(found["held"].Text, want) {
			t.Errorf("the held pipeline row reads %q, with no %q", found["held"].Text, want)
		}
	}
	if r := found["owner"]; !strings.Contains(r.Text, "held by opal") || strings.Contains(r.Text, "waiting on") {
		t.Errorf("the unheld pipeline row reads %q, want its owner alone", r.Text)
	}
	for _, r := range found {
		if !panelCells(r, "div arow-head") || panelCells(r, "div arow-body") {
			t.Errorf("the pipeline row %q has the cells %v, want its header line and nothing under it", r.Text, r.Cells)
		}
	}
}

// A row folds on its own: the click takes the words under its header line off
// the pane and the next click brings them back, one row at a time, so one long
// question stops taking the pane the rows beside it share (gt-1kaed).
func TestARowFoldsOnItsOwnAndUnfolds(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))
	switchOwner(t, dom, "overseer") // the panel opens on the human's rows; the others are one click away

	// Nothing is folded to start with, and the question's own words are all
	// drawn: the row is open until the reader says otherwise.
	if r := readRow(t, dom, 0); r.Expanded != "true" || !strings.Contains(r.Text, "how many times should a push retry?") {
		t.Errorf("the question row reads %+v before a click, want it open", r)
	}

	clickRow(t, dom, 0)
	q := readRow(t, dom, 0)
	// The header line is the row: its owner, its bead and title, its priority
	// and state, its age and its caret.
	for _, want := range []string{"operator", "gt-q1", "retry policy", "P1", "awaiting you", "1h30m", "▸"} {
		if !strings.Contains(q.Text, want) {
			t.Errorf("the folded row reads %q, with no %q of its header line", q.Text, want)
		}
	}
	for _, gone := range []string{"how many times should a push retry?", "Recommended: retry twice", "holds 2 beads", "Held: gt-h1", `gt answer gt-q1 "<answer>"`} {
		if strings.Contains(q.Text, gone) {
			t.Errorf("the folded row still reads %q, so %q did not fold away", q.Text, gone)
		}
	}
	if q.Expanded != "false" {
		t.Errorf("the folded row reads aria-expanded=%q, want false", q.Expanded)
	}

	// One click folds one row: the escalation beside it keeps every cell it had,
	// and has none under its header line to fold (gt-1kaed).
	if e := readRow(t, dom, 1); e.Head != "div" || !strings.Contains(e.Text, "raised by gastown/polecats/opal") {
		t.Errorf("the escalation row reads %+v after the question folded, want it untouched", e)
	}

	clickRow(t, dom, 0)
	if q := readRow(t, dom, 0); q.Expanded != "true" || !strings.Contains(q.Text, "Recommended: retry twice") || !strings.Contains(q.Text, "▾") {
		t.Errorf("the row reads %+v after the second click, want the question's words back", q)
	}
}

// The answer line is a line of the row and not part of the button that folds
// it, so the operator can drag it whole to copy it (gt-1kaed).
func TestTheAnswerLineIsOutsideTheButtonThatFoldsTheRow(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const row = (` + attnRowsJS + `)[0];
		const head = Array.from(row.children).find(c => c.className === "arow-head");
		const body = Array.from(row.children).find(c => c.className === "arow-body");
		const cmd = body && Array.from(body.children).find(c => c.className === "qcmd");
		return {head: head.textContent, row: row.textContent, cmd: !!cmd,
			headFolds: typeof head.onclick === "function",
			rowFolds: typeof row.onclick === "function",
			cmdFolds: !!cmd && typeof cmd.onclick === "function"};
	})())`)
	if err != nil {
		t.Fatalf("reading the question row: %v", err)
	}
	var got struct {
		Head      string `json:"head"`
		Row       string `json:"row"`
		Cmd       bool   `json:"cmd"`
		HeadFolds bool   `json:"headFolds"`
		RowFolds  bool   `json:"rowFolds"`
		CmdFolds  bool   `json:"cmdFolds"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the question row: %v", err)
	}

	if !got.Cmd || !strings.Contains(got.Row, `gt answer gt-q1 "<answer>"`) {
		t.Errorf("the row reads %q, with the answer line nowhere in it", got.Row)
	}
	for _, outside := range []string{`gt answer gt-q1 "<answer>"`, "how many times should a push retry?", "Recommended: retry twice"} {
		if strings.Contains(got.Head, outside) {
			t.Errorf("the button reads %q, holding %q the reader has to select outside it", got.Head, outside)
		}
	}
	if !got.HeadFolds {
		t.Error("the header line carries no fold, want the button the row toggles from")
	}
	if got.RowFolds || got.CmdFolds {
		t.Errorf("the row's own line folds the row (row %v, answer line %v), want the header line alone", got.RowFolds, got.CmdFolds)
	}
}

// The fold is the reader's and outlives the drawing of it: the next publish
// redraws the panel with the row still folded, and a reload reads the fold back
// out of localStorage. A row that only aged is not a row that changed (gt-1kaed).
func TestAFoldSurvivesARerenderAndAReload(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))
	switchOwner(t, dom, "overseer")
	clickRow(t, dom, 0)

	// The panel is redrawn from every publish, and the reading behind the rows
	// is not the one they were drawn from: the fold is keyed off the item and
	// what its row says, not off the row object the click was made on.
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))
	if r := readRow(t, dom, 0); r.Expanded != "false" || strings.Contains(r.Text, "how many times should a push retry?") {
		t.Errorf("the row reads %+v after a redraw, want it still folded", r)
	}

	// An item's age moves with every publish, so a row that only grew older is
	// the row the reader folded.
	aged := attentionFoldRows()
	aged[0].(map[string]any)["age_sec"] = 5460
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": aged}))
	if r := readRow(t, dom, 0); r.Expanded != "false" || !strings.Contains(r.Text, "1h31m") {
		t.Errorf("the aged row reads %+v, want it still folded and showing its new age", r)
	}

	// A reload is the page loaded again over the storage the last one left.
	again := newPageDOMStoring(t, dom.storage(t))
	again.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))
	if r := readRow(t, again, 0); r.Expanded != "false" || strings.Contains(r.Text, "how many times should a push retry?") {
		t.Errorf("the row reads %+v after a reload, want it still folded", r)
	}
	// And the escalation the reader never touched is drawn as it was: it has
	// nothing under its header line, so it has no fold to come back from.
	if r := readRow(t, again, 1); r.Head != "div" || !strings.Contains(r.Text, "raised by gastown/polecats/opal") {
		t.Errorf("the escalation row reads %+v after a reload, want it drawn as it was", r)
	}
}

// The header line is the button that folds a row, and only where there is
// something to fold: a caret on a row that hides nothing promises a fold that
// never comes (gt-1kaed).
func TestTheHeaderLineFoldsOnlyWhereThereIsSomethingToFold(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))
	switchOwner(t, dom, "overseer")

	// The question carries its own words under its header line; the escalation
	// and the queue item draw everything on the header line itself.
	if r := readRow(t, dom, 0); r.Head != "button" || r.Expanded != "true" {
		t.Errorf("the question row's header is a <%s> reading aria-expanded=%q, want a button reading true", r.Head, r.Expanded)
	}
	for i := 1; i <= 2; i++ {
		if r := readRow(t, dom, i); r.Head == "button" || r.Expanded != "" {
			t.Errorf("row %d's header is a <%s> reading aria-expanded=%q, want a line with no fold", i, r.Head, r.Expanded)
		}
	}

	clickRow(t, dom, 0)
	if r := readRow(t, dom, 0); r.Head != "button" || r.Expanded != "false" {
		t.Errorf("the folded row's header is a <%s> reading aria-expanded=%q, want a button reading false", r.Head, r.Expanded)
	}
}

// Folding a row folds it where it stands: the button the reader pressed is
// still on the page and still holding the keyboard, so the next Enter folds the
// next row (gt-1kaed).
func TestTheKeyboardStaysOnTheRowTheReaderFolded(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const head = Array.from((` + attnRowsJS + `)[0].children).find(c => c.className === "arow-head");
		__click(head);
		const after = Array.from((` + attnRowsJS + `)[0].children).find(c => c.className === "arow-head");
		return {same: after === head, focused: document.activeElement === after, expanded: after.getAttribute("aria-expanded")};
	})())`)
	if err != nil {
		t.Fatalf("reading where the keyboard is: %v", err)
	}
	var got struct {
		Same     bool   `json:"same"`
		Focused  bool   `json:"focused"`
		Expanded string `json:"expanded"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading where the keyboard is: %v", err)
	}
	if got.Expanded != "false" {
		t.Fatalf("the row reads aria-expanded=%q after the click, want it folded", got.Expanded)
	}
	if !got.Same {
		t.Error("the fold drew another button, want the row the reader pressed left where it is")
	}
	if !got.Focused {
		t.Error("the fold took the keyboard off the row the reader folded")
	}
}

// A row the reader could not fold is not a row they lost: folding changes what
// the row shows, and nothing the panel counts off the list (gt-1kaed).
func TestTheHeaderCountsDoNotMoveWhenARowFolds(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))

	note, chips := dom.element(t, "attentionnote").Text, chipReads(t, dom)
	if note == "" || len(chips) != 2 {
		t.Fatalf("the panel reads %q with the chips %v before a fold, want it counting the list", note, chips)
	}
	clickRow(t, dom, 0)
	if now := dom.element(t, "attentionnote").Text; now != note {
		t.Errorf("the note reads %q after a fold, want the %q it read before", now, note)
	}
	if now := chipReads(t, dom); !slices.Equal(now, chips) {
		t.Errorf("the chips read %v after a fold, want the %v they read before", now, chips)
	}
}

// Storage the page cannot use leaves every row open: an unreadable store holds
// no fold to draw, and a store that refuses the write keeps none for the next
// load. Neither is a reason for the pane to stop drawing (gt-1kaed).
func TestAFoldIsNotKeptWhereStorageFails(t *testing.T) {
	t.Parallel()

	// A stored value that is not the map the page wrote — a browser whose
	// storage was emptied by hand or by another page — is no fold at all.
	dom := newPageDOMStoring(t, map[string]string{"gt-attn-collapsed": "{oops"})
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))
	switchOwner(t, dom, "overseer")
	// The question reads open; the two rows beside it have no fold to read.
	if folds := rowFolds(t, dom); !slices.Equal(folds, []string{"true", "", ""}) {
		t.Errorf("the rows read %v over unreadable storage, want the question open and no fold elsewhere", folds)
	}

	// The page goes on drawing over a store that refuses the write: the click is
	// the reader's for as long as the page is up, and nothing is kept for the
	// load after it, which draws every row open.
	broken := newPageDOM(t)
	if _, err := broken.vm.RunString(`localStorage.setItem = function () { throw new Error("storage is full"); };`); err != nil {
		t.Fatalf("breaking the browser's storage: %v", err)
	}
	broken.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))
	clickRow(t, broken, 0)
	if folds := rowFolds(t, broken); !slices.Equal(folds, []string{"false"}) {
		t.Errorf("the panel's rows read %v after a click over a refused write, want the fold the reader asked for", folds)
	}
	if folds := storedFolds(t, broken); len(folds) != 0 {
		t.Errorf("the page kept %v with a store that refused the write", folds)
	}
	again := newPageDOMStoring(t, broken.storage(t))
	again.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))
	if folds := rowFolds(t, again); !slices.Equal(folds, []string{"true"}) {
		t.Errorf("the rows read %v after a reload over a store that refused the write, want them all open", folds)
	}
}

// Two tabs are one reader: a page writes the folds it made over whatever the
// store holds at that moment, so the second tab folding a row of its own does
// not write away a fold the first tab set. What the second tab draws is what it
// read at load, which a reload brings up to date (gt-1kaed).
func TestASecondTabDoesNotUndoTheFirstTabsFold(t *testing.T) {
	t.Parallel()

	twoQuestions := func() map[string]any {
		return attentionState(map[string]any{"rows": []any{
			map[string]any{"kind": "question", "owner": "operator", "id": "gt-q1", "title": "retry policy",
				"priority": 1, "question": "how many times should a push retry?", "age_sec": 60},
			map[string]any{"kind": "question", "owner": "operator", "id": "gt-q2", "title": "which slot",
				"priority": 2, "question": "which slot should the gate use?", "age_sec": 90},
		}})
	}

	// The two tabs are open over one store and neither has folded anything yet.
	first, second := newPageDOM(t), newPageDOM(t)
	first.call(t, "renderAttention", twoQuestions())
	clickRow(t, first, 0)

	// The first tab's fold lands in the store while the second tab holds the
	// page, which is a key the second one has never seen.
	handStore(t, first, second)
	second.call(t, "renderAttention", twoQuestions())
	clickRow(t, second, 1)

	if folds := storedFolds(t, second); len(folds) != 2 {
		t.Errorf("the store holds %v, want both tabs' folds", folds)
	}
	// A reload draws the row each tab folded, folded.
	reload := newPageDOMStoring(t, map[string]string{"gt-attn-collapsed": storedFoldsJSON(t, second)})
	reload.call(t, "renderAttention", twoQuestions())
	if folds := rowFolds(t, reload); !slices.Equal(folds, []string{"false", "false"}) {
		t.Errorf("the rows read %v after the reload, want the two rows the reader folded", folds)
	}
}

// A list that came back empty, or short by a read that failed or has not
// reported, is not a list an item left, so the folds are kept for the read that
// has not landed yet (gt-1kaed).
func TestAShortOrEmptyListKeepsTheFoldsItCannotAccountFor(t *testing.T) {
	t.Parallel()

	rows := attentionFoldRows()
	cases := []struct {
		name string
		at   map[string]any
	}{
		{"a list that came back empty", map[string]any{"rows": []any{}}},
		{"a list short by a read that failed", map[string]any{"rows": []any{rows[1], rows[2]}, "failed": []any{"questions"}}},
		{"a list short by a read that has not reported", map[string]any{"rows": []any{rows[1], rows[2]}, "pending": []any{"questions"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderAttention", attentionState(map[string]any{"rows": rows}))
			clickRow(t, dom, 0)

			dom.call(t, "renderAttention", attentionState(tc.at))
			if !foldOfItem(t, dom, "question:gt-q1") {
				t.Error("the page dropped the fold beside a list it could not account for")
			}
		})
	}

	// The question comes back on the next publish with the reader's fold on it.
	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": rows}))
	clickRow(t, dom, 0)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{}}))
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": rows}))
	if r := readRow(t, dom, 0); r.Expanded != "false" {
		t.Errorf("the row reads %+v when the list came back, want the fold the reader set", r)
	}
}

// A fold is set against the words the reader folded, not against the item for
// good: a question whose answer changed, or whose header line moved, comes back
// open, and an item that leaves the list takes its fold with it (gt-1kaed).
func TestARowThatSaysSomethingNewComesBackOpen(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": attentionFoldRows()}))
	clickRow(t, dom, 0)
	if r := readRow(t, dom, 0); r.Expanded != "false" {
		t.Fatalf("the question row reads %+v after the click, want it folded", r)
	}
	if !foldOfItem(t, dom, "question:gt-q1") {
		t.Fatal("the page kept no fold for the question it just folded")
	}

	// The question the reader folded is answered: the same item, new words, and
	// the row comes back open on them.
	answered := attentionFoldRows()
	answered[0].(map[string]any)["recommended"] = "retry three times\nand page someone after an hour"
	answered[0].(map[string]any)["state"] = "answered"
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": answered}))
	r := readRow(t, dom, 0)
	if r.Expanded != "true" || !strings.Contains(r.Text, "Recommended: retry three times") {
		t.Errorf("the answered row reads %+v, want it open on the answer it was given", r)
	}
	if foldOfItem(t, dom, "question:gt-q1") {
		t.Error("the page still holds the fold the reader set against the answer that was replaced")
	}

	// A change the row carries on its own header line opens it too: the fold
	// belongs to the words the reader folded, not to the item for good.
	clickRow(t, dom, 0)
	if r := readRow(t, dom, 0); r.Expanded != "false" {
		t.Fatalf("the question row reads %+v after the click, want it folded", r)
	}
	moved := attentionFoldRows()
	moved[0].(map[string]any)["priority"] = 0
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": moved}))
	if r := readRow(t, dom, 0); r.Expanded != "true" || !strings.Contains(r.Text, "P0") {
		t.Errorf("the row reads %+v after its priority moved, want it open on the new priority", r)
	}

	// The row the queue cleared takes its fold with it: a key whose item is gone
	// is dropped on the next render rather than waiting for the id to come back.
	clickRow(t, dom, 0)
	if !foldOfItem(t, dom, "question:gt-q1") {
		t.Fatal("the page kept no fold for the question it just folded")
	}
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{
		attentionFoldRows()[1], attentionFoldRows()[2],
	}}))
	if folds := storedFolds(t, dom); len(folds) != 0 {
		t.Errorf("the page holds %v after the question left the list, want no fold for an item that is gone", folds)
	}
}

// A question the reader could not parse is still a row — a question the
// operator cannot answer from the page is one they should still know about —
// and it says which part of the bead is missing (gt-bi6w0).
func TestThePanelNamesAQuestionItCannotDraw(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{
		map[string]any{"kind": "question", "owner": "operator", "id": "gt-q2", "title": "a question the bead lost",
			"state": "awaiting", "malformed": true, "age_sec": 3600},
	}}))

	rows := panelRows(t, dom)
	if len(rows) != 1 {
		t.Fatalf("the panel drew %d rows, want the malformed question", len(rows))
	}
	if !strings.Contains(rows[0].Text, "the bead's description has no question and recommended answer") {
		t.Errorf("the row reads %q, want the missing sections named", rows[0].Text)
	}
	if !strings.Contains(rows[0].Text, `gt answer gt-q2 "<answer>"`) {
		t.Errorf("the row reads %q, want the answer line the bead id gives it", rows[0].Text)
	}
}

// Nothing the human owns collapses to one line, and a read that failed or has
// not reported is named rather than drawn as that line: "nothing needs you" and
// "the list could not be read" are different things to tell the operator
// (gt-bi6w0).
func TestThePanelCollapsesWhenNothingNeedsTheHuman(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		at   map[string]any
		want []string
		gone []string
	}{
		{
			name: "nothing waiting",
			at:   map[string]any{"rows": []any{}},
			want: []string{"nothing needs you"},
			gone: []string{"have not been read yet", "unavailable"},
		},
		{
			name: "rows only the overseer has to move are not the human's list",
			at: map[string]any{"rows": []any{
				map[string]any{"kind": "escalation", "owner": "overseer", "id": "hq-9", "title": "slot held"},
			}},
			want: []string{"nothing needs you"},
		},
		{
			name: "a read that failed is named",
			at:   map[string]any{"rows": []any{}, "failed": []any{"escalations"}},
			want: []string{"unavailable: the escalations could not be read"},
			gone: []string{"nothing needs you"},
		},
		{
			name: "a reader that has not reported is not one that found nothing",
			at:   map[string]any{"rows": []any{}, "pending": []any{"escalations", "questions"}},
			want: []string{"the escalations and the questions have not been read yet"},
			gone: []string{"nothing needs you"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderAttention", attentionState(tc.at))
			text := dom.element(t, "attention").Text
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("the panel reads %q, with no %q", text, want)
				}
			}
			for _, gone := range tc.gone {
				if strings.Contains(text, gone) {
					t.Errorf("the panel reads %q, holding %q", text, gone)
				}
			}
		})
	}

	// A list that has not been built at all is neither: the readers behind it
	// have not run yet.
	dom := newPageDOM(t)
	dom.call(t, "renderAttention", map[string]any{})
	if text := dom.element(t, "attention").Text; !strings.Contains(text, "reading…") {
		t.Errorf("the panel reads %q before a list has been built, want it reading", text)
	}
}

// The note counts the rows the panel is showing and names the rows the readers
// capped out of the list, and past four hours it marks how long the oldest
// question has waited: that question is the row type that stops the town
// (gt-bi6w0).
func TestThePanelNoteCountsTheRowsAndAgesTheOldestQuestion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		rows       []any
		more       int
		wantNote   string
		wantClass  string
		wantAbsent string
	}{
		{
			name:      "one row of a longer queue",
			rows:      []any{map[string]any{"kind": "question", "owner": "operator", "id": "gt-q1", "title": "retry policy"}},
			more:      2,
			wantNote:  "1 need you · 0 in the pipeline · 1 of 3",
			wantClass: "r",
		},
		{
			name: "a question past four hours is marked",
			rows: []any{map[string]any{"kind": "question", "owner": "operator", "id": "gt-q1",
				"title": "retry policy", "age_sec": 5 * 3600}},
			wantNote:  "1 need you · 0 in the pipeline · oldest question 5h00m",
			wantClass: "r warnc",
		},
		{
			name:       "a fresh question is not",
			rows:       []any{map[string]any{"kind": "question", "owner": "operator", "id": "gt-q1", "title": "retry policy", "age_sec": 600}},
			wantNote:   "1 need you · 0 in the pipeline",
			wantClass:  "r",
			wantAbsent: "oldest question",
		},
		{
			// The cap belongs to the list the readers read, so it is counted
			// beside the shown rows only while the filter is showing all of them.
			name: "rows the filter is holding back are not counted",
			rows: []any{
				map[string]any{"kind": "question", "owner": "operator", "id": "gt-q1", "title": "retry policy"},
				map[string]any{"kind": "escalation", "owner": "overseer", "id": "hq-9", "title": "slot held"},
			},
			more:      3,
			wantNote:  "1 need you · 0 in the pipeline",
			wantClass: "r",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			at := map[string]any{"rows": tc.rows}
			if tc.more > 0 {
				at["more"] = tc.more
			}
			dom.call(t, "renderAttention", attentionState(at))
			note := dom.element(t, "attentionnote")
			if note.Text != tc.wantNote {
				t.Errorf("the note reads %q, want %q", note.Text, tc.wantNote)
			}
			if note.Class != tc.wantClass {
				t.Errorf("the note wears %q, want %q", note.Class, tc.wantClass)
			}
			if tc.wantAbsent != "" && strings.Contains(note.Text, tc.wantAbsent) {
				t.Errorf("the note reads %q, holding %q", note.Text, tc.wantAbsent)
			}
		})
	}
}

// The strip's "needs you" is the human's share of the same payload the panel
// draws: a count taken beside rows the readers capped out is a floor the strip
// draws as one, and a floor of none is a count nobody made rather than a calm
// zero (gt-bi6w0).
func TestTheStripDrawsTheHumanCountAsAFloor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		at   map[string]any
		want string
	}{
		{
			name: "every row was read",
			at:   map[string]any{"mine": 2, "rows": []any{map[string]any{"owner": "operator"}}},
			want: "2 needs you",
		},
		{
			name: "a reader capped rows out of the list",
			at:   map[string]any{"mine": 2, "more": 3, "rows": []any{map[string]any{"owner": "operator"}}},
			want: "2+ needs you",
		},
		{
			name: "a cap with no human row read is a count nobody made",
			at:   map[string]any{"mine": 0, "more": 3, "rows": []any{map[string]any{"owner": "overseer"}}},
			want: "? needs you",
		},
		{
			name: "nothing waiting is a zero the strip can say",
			at:   map[string]any{"mine": 0, "rows": []any{}},
			want: "0 needs you",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderStrip", map[string]any{"attention": tc.at})
			if text := dom.element(t, "strip").Text; !strings.Contains(text, tc.want) {
				t.Errorf("the strip reads %q, with no %q", text, tc.want)
			}
		})
	}
}

// The strip's needs-you pill counts only the rows that need a person: a row in
// the pipeline is work already in flight and clears without the human, so a
// pipeline item leaves the pill's figures where they were (gt-tv5v2).
func TestTheStripPillCountsOnlyTheRowsThatNeedAPerson(t *testing.T) {
	t.Parallel()

	act := map[string]any{"kind": "item", "owner": "operator", "class": "act", "id": "host:disk", "title": "disk"}
	pipe := map[string]any{"kind": "item", "owner": "overseer", "class": "pipeline", "id": "rejected-twice:gt-9", "title": "rework",
		"held_by": "opal", "waiting_on": "rework by opal, attempt 2"}

	// readPill draws the strip over one reading and returns the needs-you fact
	// as the operator reads it: its value and the title behind it. The feed's
	// mine is the rows that need a person which the human owns, so the fixture
	// counts it the way the reader does rather than pinning a number a miscount
	// would leave standing.
	readPill := func(rows []any) (value, tip string) {
		t.Helper()

		mine := 0
		for _, row := range rows {
			if m, ok := row.(map[string]any); ok && m["owner"] == "operator" && m["class"] != "pipeline" {
				mine++
			}
		}
		dom := newPageDOM(t)
		dom.call(t, "renderStrip", map[string]any{"attention": map[string]any{"rows": rows, "mine": mine}})
		v, err := dom.vm.RunString(`JSON.stringify((() => {
			const f = Array.from(__byId["strip"].children).find(c => c.textContent.indexOf("needs you") >= 0);
			return f ? {value: f.textContent, tip: f.title} : null;
		})())`)
		if err != nil {
			t.Fatalf("reading the strip's needs-you fact: %v", err)
		}
		var got struct {
			Value string `json:"value"`
			Tip   string `json:"tip"`
		}
		if v.String() == "null" || json.Unmarshal([]byte(v.String()), &got) != nil {
			t.Fatalf("the strip drew no needs-you fact: %s", v.String())
		}
		return got.Value, got.Tip
	}

	aloneValue, aloneTip := readPill([]any{act})
	if aloneValue != "1 needs you" {
		t.Errorf("the pill reads %q, want the human's one row", aloneValue)
	}
	if !strings.Contains(aloneTip, "1 of the 1 rows needing a person") {
		t.Errorf("the pill titles %q, want it counting the row that needs a person", aloneTip)
	}

	// Two pipeline items join the list and neither figure moves.
	withValue, withTip := readPill([]any{act, pipe, pipe})
	if withValue != aloneValue || withTip != aloneTip {
		t.Errorf("the pill reads %q/%q with two pipeline rows, want the %q/%q it read without them",
			withValue, withTip, aloneValue, aloneTip)
	}

	// A pipeline row the human owns is still work in flight: the pill counts it
	// nowhere (gt-tv5v2).
	minePipe := map[string]any{"kind": "item", "owner": "operator", "class": "pipeline", "id": "risk:gt-7:deadbeef",
		"title": "risk", "held_by": "opal", "waiting_on": "the review round"}
	if value, _ := readPill([]any{minePipe}); value != "0 needs you" {
		t.Errorf("the pill reads %q with only the human's pipeline row standing, want 0 needs you", value)
	}
}

// The strip draws the human's count off the one merged list and off nothing
// else: a reading behind it that failed or has not come back is named rather
// than drawn as a zero, and the readers the list was merged from are not a
// second picture of the same number (gt-ywlei, gt-bi6w0).
func TestTheStripNamesTheReadsBehindTheHumanCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		at   map[string]any
		want []string
	}{
		{
			name: "no list has been built",
			at:   nil,
			want: []string{"–", "needs you"},
		},
		{
			name: "a reader that failed",
			at:   map[string]any{"rows": []any{}, "failed": []any{"escalations"}},
			want: []string{"unavailable", "needs you"},
		},
		{
			name: "a reader that has not reported",
			at:   map[string]any{"rows": []any{}, "pending": []any{"questions"}},
			want: []string{"–", "needs you"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderStrip", map[string]any{"attention": tc.at})
			text := dom.element(t, "strip").Text
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("the strip reads %q, with no %q", text, want)
				}
			}
			if strings.Contains(text, "0 needs you") {
				t.Errorf("the strip reads %q, passing a count nobody made off as none", text)
			}
		})
	}

	// The old readers are still on the payload, and the strip counts none of
	// them: a count that rose without the list is the two pictures of one number
	// the merged list replaced.
	dom := newPageDOM(t)
	dom.call(t, "renderStrip", map[string]any{
		"escalations": map[string]any{"rows": []any{map[string]any{"id": "hq-1"}, map[string]any{"id": "hq-2"}}},
		"questions":   map[string]any{"rows": []any{map[string]any{"id": "gt-q1"}}},
	})
	if text := dom.element(t, "strip").Text; !strings.Contains(text, "– needs you") {
		t.Errorf("the strip reads %q with no list built, want it reading the readers as nothing to act on", text)
	}
}

// One panel replaced three: the page holds one section for what is waiting on a
// decision or an action, and the Questions, Escalations and Alerts sections are
// gone from it (gt-bi6w0).
func TestOnePanelReplacedTheThree(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	if !strings.Contains(page, `<section><h2>Attention <span class="chips" id="attentionchips"></span> <span class="r" id="attentionnote"></span></h2><div class="body" id="attention"></div></section>`) {
		t.Error("the Attention section is not the markup its neighbours are")
	}
	for _, gone := range []string{`id="questions"`, `id="escalations"`, `id="alerts"`, `renderQuestions(`, `renderEscalations(`, `renderAlerts(`} {
		if strings.Contains(page, gone) {
			t.Errorf("index.html still holds %s, which the one panel replaced", gone)
		}
	}

	// Every cell goes through el(), which writes textContent: a question, a
	// recommendation and a queue summary are other agents' own words, and a tag
	// in them shows as that text and never as markup.
	for _, fn := range []string{"renderAttention", "attentionRow"} {
		if strings.Contains(pageFunc(t, fn), "innerHTML") {
			t.Errorf("%s sets markup from the row's text", fn)
		}
	}
	if !strings.Contains(page, `.qcmd{`) || !strings.Contains(page, `.qtext{`) {
		t.Error("index.html has no style for the question's own words or the copy-ready line")
	}
}

// End to end through the hub: the panel is the readers' own rows — the open
// escalations, the open questions and the attention items — merged with the
// owner each of them carries, and the strip counts the human's share of it
// (gt-bi6w0).
func TestTheHubMergesTheReadersIntoTheOneList(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	f := NewAttentionFollower(town)
	h := NewHub(Config{
		Now: func() time.Time { return alertBase },
		Escalation: func() *Escalations {
			return &Escalations{Rows: []EscalationRow{
				{ID: "hq-1", Title: "disk full", Severity: "critical", EscalatedBy: "gastown/polecats/opal", AgeSec: ageOfSec(600)},
			}}
		},
		Questions: func() *Questions {
			return &Questions{Rows: []QuestionRow{
				{ID: "gt-q1", Title: "retry policy", Priority: 0, Question: "retry?", Recommended: "retry twice", State: QuestionAwaiting},
			}}
		},
		Attention: f.Read,
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollEscalation()
	h.pollQuestions()
	st := h.State()
	if st.Attention == nil || len(st.Attention.Rows) != 2 {
		t.Fatalf("the list holds %+v, want the escalation and the question", st.Attention)
	}
	// A question awaits the human, and the escalation waits on the overseer
	// until it says otherwise: only the question is the human's to count.
	if st.Attention.Mine != 1 {
		t.Errorf("the human owns %d rows of %+v, want the awaiting question alone", st.Attention.Mine, st.Attention.Rows)
	}
	if st.Strip == nil || st.Strip.NeedsYou != alarmBad {
		t.Errorf("the strip wears %+v, want the human's row to raise needs you", st.Strip)
	}

	// A wake line the queue raises is a row of the same list, tagged with the
	// owner its rule names.
	writeAttentionEvents(t, town, wakeEvent(attention.RuleHostDiskCrit, "host:disk", "the volume is 96% full", attention.SeverityHigh))
	h.pollAttention()
	st = h.State()
	rows := st.Attention.Rows
	if len(rows) != 3 {
		t.Fatalf("the list holds %d rows after the wake line, want 3", len(rows))
	}
	var item *AttentionRow
	for i := range rows {
		if rows[i].Kind == AttentionItem {
			item = &rows[i]
		}
	}
	if item == nil || item.Rule != attention.RuleHostDiskCrit || item.Owner != string(attention.OwnerOperator) {
		t.Fatalf("the queue item is %+v, want the host rule owned by the human", item)
	}
	if item.Title != "the volume is 96% full" {
		t.Errorf("the item is titled %q, want the queue's own summary line", item.Title)
	}
	if st.Attention.Mine != 2 {
		t.Errorf("the human owns %d rows, want the host item beside the question", st.Attention.Mine)
	}
	// The question's own words reach the panel off the same row, so the answer
	// the operator reads is the one the overseer wrote.
	for _, r := range rows {
		if r.ID == "gt-q1" && r.Question != "retry?" {
			t.Errorf("the question row carries %q, want the question's own words", r.Question)
		}
	}

	// The cleared line takes the row off the list, and the two beads stay.
	writeAttentionEvents(t, town, clearedEvent(wakeEvent(attention.RuleHostDiskCrit, "host:disk", "the volume is 96% full", attention.SeverityHigh)))
	h.pollAttention()
	if st := h.State(); len(st.Attention.Rows) != 2 {
		t.Errorf("the list holds %+v after the clear, want the two beads", st.Attention.Rows)
	}
}

// A queue item's age is the instant the list was built against the one it was
// raised at, so a list that is only rebuilt when one of its own readers moves
// shows a wait that stopped growing: every publish rebuilds it, whichever
// reader moved (gt-bi6w0).
func TestTheHubReagesTheListOnEveryPublish(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	f := NewAttentionFollower(town)
	now := alertBase
	h := NewHub(Config{
		Now:       func() time.Time { return now },
		Health:    func() Health { return Health{Line: "all green", Verdict: "green", ReadAt: now} },
		Attention: f.Read,
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	writeAttentionEvents(t, town, wakeEvent(attention.RuleSlotHeld, "slot:gastown", "slot held 3h", attention.SeverityHigh))
	h.pollAttention()
	if got := itemAge(t, h); got != 0 {
		t.Fatalf("the item is %d seconds old at the read time, want 0", got)
	}

	// Ten minutes on, a reader with nothing to do with the list publishes: the
	// item's age is the published instant, not the one it was first drawn at.
	now = now.Add(10 * time.Minute)
	h.pollHealth()
	if got := itemAge(t, h); got != 600 {
		t.Errorf("the item is %d seconds old after ten minutes, want 600: the list was not re-aged", got)
	}
}

// itemAge reads the age of the panel's one queue item.
func itemAge(t *testing.T, h *Hub) int64 {
	t.Helper()

	st := h.State()
	if st.Attention == nil {
		t.Fatal("the hub holds no Attention list")
	}
	for _, r := range st.Attention.Rows {
		if r.Kind == AttentionItem {
			if r.AgeSec == nil {
				t.Fatal("the queue item carries no age")
			}
			return *r.AgeSec
		}
	}
	t.Fatal("the list holds no queue item")
	return 0
}

// The hub draws the panel's rows from the answers reader it was given, and names
// the read when the log could not be read: the rows keep standing either way,
// and an unread log never reads as a queue Jev answered nothing about
// (gt-b9j3o.2).
func TestTheHubScoresThePanelFromTheAnswersReader(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	f := NewAttentionFollower(town)
	fail := false
	scores := attention.Scores{attention.LifeKey("host:disk", alertBase): 0.87}
	h := NewHub(Config{
		Now:       func() time.Time { return alertBase },
		Attention: f.Read,
		NeedsHuman: func() (attention.Scores, error) {
			if fail {
				return nil, errors.New("reading attention.jsonl: permission denied")
			}
			return scores, nil
		},
	})
	writeAttentionEvents(t, town, attention.Event{TS: alertBase, Class: attention.KindRiskPath, Severity: attention.SeverityLow,
		Text: "record", Key: "risk:x", State: attention.EventNew, Rule: attention.RuleRiskPath, Handled: attention.HandledInfo})
	h.pollAttention() // opens the tail; the file held only an info line

	writeAttentionEvents(t, town, wakeEvent(attention.RuleHostDiskCrit, "host:disk", "the volume is 96% full", attention.SeverityHigh))
	h.pollAttention()
	rows := h.State().Attention.Rows
	if len(rows) != 1 {
		t.Fatalf("the panel holds %+v, want the host reading alone", rows)
	}
	if rows[0].NeedsHuman == nil || *rows[0].NeedsHuman != 0.87 {
		t.Errorf("the row carries the score %v, want the reader's 0.87", rows[0].NeedsHuman)
	}

	// The log becomes unreadable, and the next raise redraws the panel.
	fail = true
	writeAttentionEvents(t, town, wakeEvent(attention.RuleRedMain, "red-main:gastown", "main is red", attention.SeverityHigh))
	h.pollAttention()
	at := h.State().Attention
	if len(at.Rows) != 2 {
		t.Fatalf("an unreadable answers log dropped a row: %+v", at.Rows)
	}
	if !at.ScoresFailed {
		t.Error("an unreadable answers log left the panel saying the scores were read")
	}
	if at.NeedsYou.Count != 2 || len(at.Failed) != 0 {
		t.Errorf("an unreadable answers log left %d needs-you rows and failed %v, want the rows read and no failed reader",
			at.NeedsYou.Count, at.Failed)
	}
}

// /api/state carries the panel's two groups with their counts and with the
// owner and waiting-on a pipeline row names, so a consumer of the feed reads
// what needs a person apart from what is already in flight (gt-tv5v2).
func TestTheGroupsReachTheAPI(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	f := NewAttentionFollower(town)
	h := NewHub(Config{
		Now:            func() time.Time { return alertBase },
		Attention:      f.Read,
		AttentionItems: f.Items,
	})
	// The daemon's state holds the two items; the events raise the two rows.
	if err := attention.WriteState(town, attention.State{Updated: alertBase, Items: []attention.Item{
		{Key: "host:disk", Kind: attention.KindHostDisk, Severity: attention.SeverityHigh,
			Summary: "the volume is 96% full", FirstSeen: alertBase, LastSeen: alertBase},
		{Key: "rejected-twice:gt-9", Kind: attention.KindRejectedTwice, Severity: attention.SeverityHigh,
			Summary: "gt-9 was rejected twice", Owner: "opal", WaitingOn: "rework by opal, attempt 2",
			FirstSeen: alertBase, LastSeen: alertBase},
	}}); err != nil {
		t.Fatalf("writing the queue's state: %v", err)
	}
	writeAttentionEvents(t, town,
		wakeEvent(attention.RuleHostDiskCrit, "host:disk", "the volume is 96% full", attention.SeverityHigh),
		wakeEvent(attention.RuleRejectedTwice, "rejected-twice:gt-9", "gt-9 was rejected twice", attention.SeverityHigh))
	h.pollAttention()

	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /api/state = %d", rec.Code)
	}
	var got struct {
		Attention Attention `json:"attention"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("/api/state is not the page's JSON: %v", err)
	}
	if got.Attention.NeedsYou.Count != 1 || len(got.Attention.NeedsYou.Rows) != 1 ||
		got.Attention.NeedsYou.Rows[0].ID != "host:disk" {
		t.Errorf("/api/state carries the needs-you group as %+v, want the host reading alone", got.Attention.NeedsYou)
	}
	if got.Attention.Pipeline.Count != 1 || len(got.Attention.Pipeline.Rows) != 1 {
		t.Fatalf("/api/state carries the pipeline group as %+v, want the rejected bead alone", got.Attention.Pipeline)
	}
	if r := got.Attention.Pipeline.Rows[0]; r.ID != "rejected-twice:gt-9" || r.HeldBy != "opal" ||
		r.WaitingOn != "rework by opal, attempt 2" {
		t.Errorf("the pipeline row reaches the feed as %+v, want the queue's owner and waiting-on line", r)
	}
}

// The page reads the list off /api/state, so a panel that renders it needs no
// query of its own.
func TestTheListReachesTheAPI(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{
		Now: func() time.Time { return questionPageNow },
		Escalation: func() *Escalations {
			return &Escalations{Rows: []EscalationRow{{ID: "hq-1", Title: "disk full", Severity: "high"}}}
		},
	})
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)

	// Nothing has been read yet, so the page is told the list is not there
	// rather than that nothing is waiting.
	var cold struct {
		Attention *Attention `json:"attention"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cold); err != nil {
		t.Fatalf("/api/state is not the page's JSON: %v", err)
	}
	if cold.Attention != nil {
		t.Errorf("/api/state carries %+v before a reader has reported", cold.Attention)
	}

	h.pollEscalation()
	rec = httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	var warm struct {
		Attention Attention `json:"attention"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &warm); err != nil {
		t.Fatalf("/api/state is not the page's JSON: %v", err)
	}
	if len(warm.Attention.Rows) != 1 || warm.Attention.Rows[0].ID != "hq-1" {
		t.Fatalf("/api/state carries %+v, want the escalation row", warm.Attention.Rows)
	}
	// The reader that has not reported is named, so the panel says so rather
	// than drawing its absence as a town with no questions.
	if len(warm.Attention.Pending) != 1 || warm.Attention.Pending[0] != "questions" {
		t.Errorf("/api/state names %v pending, want the questions reader", warm.Attention.Pending)
	}
}
