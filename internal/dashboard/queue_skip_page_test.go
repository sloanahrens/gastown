package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

// The queue's skipped rows are drawn by index.html's own script, so the tests
// below run that script over the reading the server would send
// (page_harness_test.go) rather than matching its source: a render function
// that mentions SkipReason draws nothing (gt-tq4ee, gt-e2cif).

// queuePageJS reads the drawn queue back: the list the panel drew, its chips,
// the rows inside it, the note above them and the placeholder an empty list
// shows. The class names are the page's own.
const queuePageJS = `
function __qBox() {
  let box = null;
  __walk(__byId["work"], n => { if (!box && n.className === "qlist") box = n; });
  return box;
}
function __qRowsBox() {
  const box = __qBox();
  if (!box) return null;
  for (let i = 0; i < box.children.length; i++) if (box.children[i].className === "qrows") return box.children[i];
  return null;
}
function __qChips() {
  const out = [], box = __qBox();
  if (box) __walk(box, n => {
    if ((" " + n.className + " ").indexOf(" chip ") < 0) return;
    out.push({label: String(n._text || ""), count: n.children.length ? String(n.children[0].textContent) : "", title: n.title || "", on: n.className.split(" ").indexOf("on") >= 0});
  });
  return out;
}
function __qRows() {
  const out = [], rows = __qRowsBox();
  if (rows) for (const r of rows.children) {
    if (r.className !== "qrow") continue;
    const leaves = [];
    __walk(r, k => {
      if (k.nodeType === 3 || k.children.length || String(k.textContent) === "") return;
      leaves.push({tag: String(k.tagName).toLowerCase(), class: k.className, text: String(k.textContent)});
    });
    out.push({text: String(r.textContent), leaves: leaves});
  }
  return out;
}
function __qNotes() {
  const out = [], box = __qBox();
  if (box) for (const c of box.children) if ((" " + c.className + " ").indexOf(" warnc ") >= 0) out.push(String(c.textContent));
  return out;
}
function __qEmpty() {
  const rows = __qRowsBox();
  if (rows) for (const c of rows.children) if ((" " + c.className + " ").indexOf(" empty ") >= 0) return String(c.textContent);
  return "";
}
function __qClick(label) {
  const box = __qBox();
  let hit = null;
  if (box) __walk(box, n => {
    if (!hit && (" " + n.className + " ").indexOf(" chip ") >= 0 && String(n._text || "").indexOf(label) === 0) hit = n;
  });
  if (!hit) throw new Error("the queue draws no chip " + label);
  __click(hit);
}
`

// queueChipDrawn is one chip of the queue's filter row as the page drew it: the
// label the page wrote, the count it put beside it, and whether it is the open
// filter. The stub's textContent reads a parent's children and not the label a
// page wrote before appending them, so the label is read where it was set.
type queueChipDrawn struct {
	Label string `json:"label"`
	Count string `json:"count"`
	Title string `json:"title"`
	On    bool   `json:"on"`
}

// queueLeafDrawn is one leaf cell of a queue row.
type queueLeafDrawn struct {
	Tag   string `json:"tag"`
	Class string `json:"class"`
	Text  string `json:"text"`
}

// queueRowDrawn is one row of the queue as the page drew it: the text it reads
// and the leaf cells under it.
type queueRowDrawn struct {
	Text   string           `json:"text"`
	Leaves []queueLeafDrawn `json:"leaves"`
}

// drawQueuePage runs index.html over one queue reading and draws the Work
// panel with it, the way a poll does: what a test reads back is what an operator
// sees, chips and rows alike.
func drawQueuePage(t *testing.T, queue map[string]any) *pageDOM {
	t.Helper()

	dom := newPageDOM(t)
	if _, err := dom.vm.RunString(queuePageJS); err != nil {
		t.Fatalf("loading the queue's test helpers: %v", err)
	}
	q, err := json.Marshal(queue)
	if err != nil {
		t.Fatalf("marshalling the reading: %v", err)
	}
	js := `state = {summary_at: "2026-10-29T12:00:00Z", summary: {ready_to_land: 0}, polecats: [], queue: ` + string(q) + `};
		renderWork(state, true);`
	if _, err := dom.vm.RunString(js); err != nil {
		t.Fatalf("drawing the work panel over a queue: %v", err)
	}
	return dom
}

// queueRead is a ready list of the given rows with the counts a read carries
// beside them.
func queueRead(rows ...map[string]any) map[string]any {
	return map[string]any{
		"at": "2026-10-29T12:00:00Z", "ready_total": len(rows), "ready": rows,
		"landing_total": 0, "blocked_total": 0, "blocked_work": 0, "blocked_wisps": 0,
	}
}

// readyRow is one ready bead of the reading, shaped as the dispatcher left it.
func readyRow(id, shape string) map[string]any {
	return map[string]any{"id": id, "title": id + " work", "priority": 2, "rig": "gastown", "shape": shape}
}

// skippedRow is a ready bead the dispatcher's last tick passed over: it lints
// "ok" and still waits, and the verdict says why and since when.
func skippedRow(id, since, reason string, ticks int) map[string]any {
	row := readyRow(id, "skipped")
	row["shape_note"] = reason
	row["skip_class"] = "skipped"
	row["skip_reason"] = reason
	row["skipped_since"] = since
	row["skip_ticks"] = ticks
	return row
}

// queueReadBack reads one of the queue helpers over the page.
func queueReadBack(t *testing.T, dom *pageDOM, expr string, out any) {
	t.Helper()

	v, err := dom.vm.RunString("JSON.stringify(" + expr + ")")
	if err != nil {
		t.Fatalf("reading %s: %v", expr, err)
	}
	if err := json.Unmarshal([]byte(v.String()), out); err != nil {
		t.Fatalf("reading %s: %v", expr, err)
	}
}

func queueChips(t *testing.T, dom *pageDOM) []queueChipDrawn {
	t.Helper()

	var out []queueChipDrawn
	queueReadBack(t, dom, "__qChips()", &out)
	return out
}

func queueRows(t *testing.T, dom *pageDOM) []queueRowDrawn {
	t.Helper()

	var out []queueRowDrawn
	queueReadBack(t, dom, "__qRows()", &out)
	return out
}

func queueNotes(t *testing.T, dom *pageDOM) []string {
	t.Helper()

	var out []string
	queueReadBack(t, dom, "__qNotes()", &out)
	return out
}

// clickQueueChip presses one filter chip, the way the operator does.
func clickQueueChip(t *testing.T, dom *pageDOM, label string) {
	t.Helper()

	if _, err := dom.vm.RunString("__qClick(" + quoteJS(label) + ")"); err != nil {
		t.Fatalf("pressing the %q chip: %v", label, err)
	}
}

// chipNamed reads one chip of the queue, found by the label it starts with. A
// chip the page no longer draws fails here rather than reading as a zero count.
func chipNamed(t *testing.T, chips []queueChipDrawn, label string) queueChipDrawn {
	t.Helper()

	for _, c := range chips {
		if strings.HasPrefix(c.Label, label) {
			return c
		}
	}
	t.Fatalf("the queue draws no %q chip; it draws %+v", label, chips)
	return queueChipDrawn{}
}

// leafSaying reads the first leaf of a row of the given class whose text holds
// want, so a test names the line it means rather than a position among them.
func leafSaying(row queueRowDrawn, class, want string) (queueLeafDrawn, bool) {
	for _, l := range row.Leaves {
		if l.Class == class && strings.Contains(l.Text, want) {
			return l, true
		}
	}
	return queueLeafDrawn{}, false
}

// localClock is a time as the page writes it on a row: the viewer's own clock,
// which is what the frozen browser the harness runs in makes deterministic.
func localClock(t *testing.T, dom *pageDOM, iso string) string {
	t.Helper()

	v, err := dom.vm.RunString(`new Date(` + quoteJS(iso) + `).toLocaleTimeString([], {hour12: false})`)
	if err != nil {
		t.Fatalf("reading the viewer's clock: %v", err)
	}
	return v.String()
}

// The dispatchable chip counts the beads the dispatcher would slot, and a bead
// it passed over is not one of them: that bead is counted on a chip of its own,
// which opens its row (gt-e2cif). Both chips say what they mean.
func TestQueueSkipPageChip(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(
		readyRow("gt-okckk", "ok"),
		skippedRow("gt-skpv2", "2026-10-29T11:58:00Z", "overlap hold: gt-okckk is in progress", 12),
	))

	chips := queueChips(t, dom)
	if got := chipNamed(t, chips, "dispatchable"); got.Count != "1" {
		t.Errorf("the dispatchable chip counts %s, want only the shaped bead (1): a skipped bead is not one the dispatcher slots", got.Count)
	}
	skipped := chipNamed(t, chips, "skipped by dispatcher")
	if skipped.Count != "1" {
		t.Errorf("the skipped chip counts %s, want the one bead the dispatcher passed over", skipped.Count)
	}
	if !strings.Contains(skipped.Title, "dispatcher") {
		t.Errorf("the skipped chip's tooltip reads %q, want it to say whose verdict it counts", skipped.Title)
	}
	if skipped.On {
		t.Error("the skipped chip is the open filter on a fresh page, which opens on the dispatchable view")
	}

	// The page opens on the dispatchable view, and the skipped bead is not in
	// it: it is one chip away.
	for _, r := range queueRows(t, dom) {
		if strings.Contains(r.Text, "gt-skpv2") {
			t.Errorf("the dispatchable view draws the skipped bead: %q", r.Text)
		}
	}
	clickQueueChip(t, dom, "skipped by dispatcher")
	rows := queueRows(t, dom)
	if len(rows) != 1 || !strings.Contains(rows[0].Text, "gt-skpv2") {
		t.Fatalf("the skipped chip draws %+v, want the one skipped bead", rows)
	}
}

// A ready list the dispatcher skipped in full leaves the dispatchable view
// empty, so that view names the skips instead of reporting that nothing needs
// shaping and nothing is held (gt-e2cif).
func TestQueueSkipPageEmptyDispatchableView(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(
		skippedRow("gt-skpv2", "2026-10-29T11:58:00Z", "overlap hold: gt-okckk is in progress", 12),
	))

	var msg string
	queueReadBack(t, dom, "__qEmpty()", &msg)
	if want := "1 bead was skipped by the dispatcher"; !strings.Contains(msg, want) {
		t.Errorf("the empty dispatchable view reads %q, want it to name the skip (%q)", msg, want)
	}
}

// The row of a bead the dispatcher passed over says why and since when: the
// class it filed it under, the time in the viewer's own clock, how many ticks
// the verdict has stood, and the tick's own reason — all of it as text, since a
// reason is another agent's line (gt-e2cif).
func TestQueueSkipPageRowReason(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(
		skippedRow("gt-skpv2", "2026-10-29T11:58:00Z", "overlap hold: gt-okckk is in progress", 12),
	))
	clickQueueChip(t, dom, "skipped by dispatcher")

	rows := queueRows(t, dom)
	if len(rows) != 1 {
		t.Fatalf("the skipped chip draws %d rows, want the one bead: %+v", len(rows), rows)
	}
	want := "skipped since " + localClock(t, dom, "2026-10-29T11:58:00Z") + " for 12 ticks: overlap hold: gt-okckk is in progress"
	line, ok := leafSaying(rows[0], "qsub", "overlap hold")
	if !ok {
		t.Fatalf("the row draws no dim line saying why it was passed over; its cells are %+v", rows[0].Leaves)
	}
	if line.Text != want {
		t.Errorf("the reason line reads %q, want %q", line.Text, want)
	}
	if line.Tag != "span" {
		t.Errorf("the reason line is a <%s>, want the row's own dim span", line.Tag)
	}
	// The chip that opens the row says what the bead is, so the row need not be
	// read twice to find out.
	for _, l := range rows[0].Leaves {
		if l.Class == "tag warnc" && l.Text == "skipped by dispatcher" {
			return
		}
	}
	t.Errorf("the skipped row carries no %q tag; its cells are %+v", "skipped by dispatcher", rows[0].Leaves)
}

// A reason is text another agent wrote, so the page writes it as text: markup
// in a reason reads as the characters it is, never as an element (gt-e2cif).
func TestQueueSkipPageReasonIsTextNotMarkup(t *testing.T) {
	t.Parallel()

	const reason = `<b>held</b> & "later"`
	dom := drawQueuePage(t, queueRead(
		skippedRow("gt-evilv", "2026-10-29T11:58:00Z", reason, 1),
	))
	clickQueueChip(t, dom, "skipped by dispatcher")

	rows := queueRows(t, dom)
	if len(rows) != 1 {
		t.Fatalf("the skipped chip draws %d rows, want the one bead: %+v", len(rows), rows)
	}
	want := "skipped since " + localClock(t, dom, "2026-10-29T11:58:00Z") + " for 1 tick: " + reason
	line, ok := leafSaying(rows[0], "qsub", "<b>held</b>")
	if !ok {
		t.Fatalf("the reason line does not read the reason as written; its cells are %+v", rows[0].Leaves)
	}
	if line.Text != want {
		t.Errorf("the reason line reads %q, want %q", line.Text, want)
	}
	for _, l := range rows[0].Leaves {
		if l.Tag == "b" {
			t.Errorf("a reason was parsed as markup: the row draws a <b> holding %q", l.Text)
		}
	}
}

// A ledger the page cannot present as current says so once, above the rows, and
// that many minutes old: a verdict the dispatcher has not re-taken is not a
// current skip, so neither the chip nor the row's tag nor its line draws one
// (gt-e2cif).
func TestQueueSkipPageStaleVerdicts(t *testing.T) {
	t.Parallel()

	read := queueRead(
		readyRow("gt-okckk", "ok"),
		skippedRow("gt-skpv2", "2026-10-29T11:58:00Z", "overlap hold: gt-okckk is in progress", 12),
	)
	read["verdicts_stale"] = true
	dom := drawQueuePage(t, read)
	// The harness froze the page's clock when the stub loaded, so a tick twelve
	// minutes before it is a ledger the page can age exactly.
	if _, err := dom.vm.RunString(`state.queue.verdicts_at = new Date(__epoch - 12*60000).toISOString(); renderWork(state, true);`); err != nil {
		t.Fatalf("stamping the verdicts' time: %v", err)
	}

	notes := queueNotes(t, dom)
	if len(notes) != 1 {
		t.Fatalf("the stale ledger draws %d notes above the rows (%+v), want one", len(notes), notes)
	}
	if want := "the dispatcher's verdicts are 12 minutes old"; !strings.Contains(notes[0], want) {
		t.Errorf("the stale note reads %q, want it to say %q", notes[0], want)
	}

	// The chip is the ledger's claim in miniature, so a stale ledger draws no
	// chip counting a skip it cannot back.
	for _, c := range queueChips(t, dom) {
		if strings.HasPrefix(c.Label, "skipped by dispatcher") {
			t.Errorf("a stale ledger still draws the %q chip, counting %s skips it cannot back", c.Label, c.Count)
		}
	}

	// Nor does any row claim one. The dispatchable view opens on the shaped
	// bead, so turn its filter off to see the row the reading gave shape
	// "skipped": it draws neither the tag nor the reason line.
	clickQueueChip(t, dom, "dispatchable")
	rows := queueRows(t, dom)
	if len(rows) != 2 {
		t.Fatalf("the stale ledger draws %d rows, want both beads of the reading: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if strings.Contains(r.Text, "overlap hold") || strings.Contains(r.Text, "skipped since") {
			t.Errorf("a row claims a skip the stale ledger cannot back: %q", r.Text)
		}
		for _, l := range r.Leaves {
			if l.Text == "skipped by dispatcher" {
				t.Errorf("a row wears the skipped tag while the ledger is stale: %q", r.Text)
			}
		}
	}
}

// A stale ledger names no skip anywhere, the empty dispatchable view included:
// with every ready bead passed over the view is empty, and it must not report a
// count of skips the page cannot back (gt-e2cif).
func TestQueueSkipPageStaleEmptyViewNamesNoSkip(t *testing.T) {
	t.Parallel()

	read := queueRead(
		skippedRow("gt-skpv2", "2026-10-29T11:58:00Z", "overlap hold: gt-okckk is in progress", 12),
	)
	read["verdicts_stale"] = true
	dom := drawQueuePage(t, read)

	var msg string
	queueReadBack(t, dom, "__qEmpty()", &msg)
	if strings.Contains(msg, "skipped") {
		t.Errorf("the empty dispatchable view reports skips on a stale ledger: %q", msg)
	}
}

// A filter left on the skipped chip when the ledger goes stale would keep
// hiding rows behind a control the page no longer draws, so the page drops the
// filter and lists every row the reading holds (gt-e2cif).
func TestQueueSkipPageStaleDropsTheSkippedFilter(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(
		readyRow("gt-okckk", "ok"),
		skippedRow("gt-skpv2", "2026-10-29T11:58:00Z", "overlap hold: gt-okckk is in progress", 12),
	))
	// The operator is looking at the skipped view when the dispatcher's ledger
	// ages out: the chip was pressed while the verdicts were still current.
	clickQueueChip(t, dom, "skipped by dispatcher")
	if _, err := dom.vm.RunString(`state.queue.verdicts_stale = true; renderWork(state, true);`); err != nil {
		t.Fatalf("ageing the ledger under the skipped view: %v", err)
	}

	// The list is no longer narrowed to the shape the dropped chip filtered to:
	// both beads of the reading are listed, the shaped one included.
	if rows := queueRows(t, dom); len(rows) != 2 {
		t.Fatalf("the stale ledger draws %d rows, want both beads of the reading: %+v", len(rows), rows)
	}
}

// A ledger with no time to age says it could not be read, rather than ageing a
// time it does not have: the server omits the time of a ledger it could not
// read, and one it sent with a zero time is the JSON zero value, not an age
// (gt-e2cif).
func TestQueueSkipPageUnreadableLedger(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		at   any
	}{
		{"no time at all", nil},
		{"a zero time", "0001-01-01T00:00:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			read := queueRead(readyRow("gt-okckk", "ok"))
			read["verdicts_stale"] = true // the server's word for a ledger it could not read
			if tt.at != nil {
				read["verdicts_at"] = tt.at
			}
			dom := drawQueuePage(t, read)

			notes := queueNotes(t, dom)
			if len(notes) != 1 {
				t.Fatalf("the unreadable ledger draws %d notes above the rows (%+v), want one", len(notes), notes)
			}
			if want := "verdict ledger could not be read"; !strings.Contains(notes[0], want) {
				t.Errorf("the note reads %q, want it to say %q", notes[0], want)
			}
			if strings.Contains(notes[0], "minutes old") {
				t.Errorf("a ledger with no time is aged into minutes: %q", notes[0])
			}
		})
	}
}
