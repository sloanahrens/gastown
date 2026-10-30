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
      leaves.push({tag: String(k.tagName).toLowerCase(), class: k.className, text: String(k.textContent), title: String(k.title || "")});
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
	Title string `json:"title"`
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

// One "ready to dispatch" chip counts the beads the dispatcher would slot and
// the beads its last tick passed over together, and its filter lists both; the
// old "dispatchable" and "skipped by dispatcher" chips are gone (gt-qt0ym).
func TestQueueSkipPageOneReadyToDispatchChip(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(
		readyRow("gt-okckk", "ok"),
		skippedRow("gt-skpv2", "2026-10-29T11:58:00Z", "overlap hold: gt-okckk is in progress", 12),
		skippedRow("gt-seat1", "2026-10-29T11:58:00Z", "no seat: over the town total", 4),
	))

	chips := queueChips(t, dom)
	if got := chipNamed(t, chips, "ready to dispatch"); got.Count != "3" || !got.On {
		t.Errorf("the ready to dispatch chip reads %+v, want count 3 (1 shaped + 2 skipped) and open on a fresh page", got)
	}
	for _, c := range chips {
		if strings.HasPrefix(c.Label, "dispatchable") || strings.HasPrefix(c.Label, "skipped by dispatcher") {
			t.Errorf("the queue still draws the old chip %q", c.Label)
		}
	}
	if rows := queueRows(t, dom); len(rows) != 3 {
		t.Fatalf("the ready to dispatch view draws %d rows, want all 3: %+v", len(rows), rows)
	}
}

// skipClass maps a tick's skip reason to the words an operator reads: a reason
// beginning "no seat" is a bead waiting for a seat, every other reason, an
// empty one included, is a skip that stays visible as a warning (gt-qt0ym).
func TestSkipClass(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	for _, tt := range []struct{ name, reason, want string }{
		{"no seat", "no seat: over the town total", "waiting for a seat"},
		{"no seat for the model", "no seat free for deepseek-flash", "waiting for a seat"},
		{"not eligible", "not eligible (assigned to gastown/polecats/slit)", "skipped"},
		{"unshaped", "unshaped: missing ## Gate", "skipped"},
		{"hold", "hold: ~/gt/seat-refill.hold is present", "skipped"},
		{"overlap hold", "overlap hold: gt-okckk is in progress", "skipped"},
		{"empty", "", "skipped"},
		// The live reasons carry the bead id first (gt-fvtw7).
		{"live no seat", "gt-vesnm: no seat: over the town total: 14/12 live polecats across every seat", "waiting for a seat"},
		{"id-prefixed dotted id", "gt-tpg0p.70: no seat: over the town total", "waiting for a seat"},
		{"id-prefixed not eligible", "gt-vesnm: not eligible (assigned to gastown/polecats/slit)", "skipped"},
		{"id-prefixed unshaped", "gt-vesnm: unshaped: missing ## Gate", "skipped"},
		{"id-prefixed held", "gt-vesnm: held: overlap with gt-okckk", "skipped"},
		{"id-prefixed container", "gt-vesnm: container: open children", "skipped"},
		{"id-prefixed rig hold", "gt-vesnm: rig hold: gastown is parked", "skipped"},
		{"id-prefixed per-tick limit", "gt-vesnm: clean, per-tick limit 5", "skipped"},
		{"id-prefixed startup backoff", "gt-vesnm: startup backoff: 3m left", "skipped"},
		{"id-prefixed blocked", "gt-vesnm: blocked by gt-okckk", "skipped"},
		// A reason word is not a bead id, so a bare reason is not cut.
		{"held is not an id", "held: no seat is free", "skipped"},
	} {
		// Not parallel: the subtests share one goja VM, which is not safe for
		// concurrent use.
		t.Run(tt.name, func(t *testing.T) {
			v, err := dom.vm.RunString("skipClass(" + quoteJS(tt.reason) + ")")
			if err != nil {
				t.Fatalf("skipClass(%q): %v", tt.reason, err)
			}
			if got := v.String(); got != tt.want {
				t.Errorf("skipClass(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}

// A bead skipped for want of a seat wears the dim tag "waiting for a seat #N of
// M", N being its place in the dispatch order (priority, then oldest, then id)
// over every waiting bead; any other skip keeps the warning tag with its
// reason, and the card body still carries the full reason line (gt-qt0ym).
func TestQueueSkipPageTagsByCause(t *testing.T) {
	t.Parallel()

	seat := func(id string, priority int, created string) map[string]any {
		row := skippedRow(id, "2026-10-29T11:58:00Z", id+": no seat: over the town total: 14/12 live polecats across every seat", 4)
		row["priority"] = priority
		row["created_at"] = created
		return row
	}
	dom := drawQueuePage(t, queueRead(
		seat("gt-late1", 2, "2026-10-29T10:00:00Z"),
		seat("gt-p1new", 1, "2026-10-29T11:00:00Z"),
		seat("gt-early", 2, "2026-10-28T10:00:00Z"),
		skippedRow("gt-skpv2", "2026-10-29T11:58:00Z", "gt-skpv2: not eligible (assigned to gastown/polecats/slit)", 2),
	))

	tags := map[string]queueRowDrawn{}
	for _, r := range queueRows(t, dom) {
		for _, id := range []string{"gt-late1", "gt-p1new", "gt-early", "gt-skpv2"} {
			if strings.Contains(r.Text, id) {
				tags[id] = r
			}
		}
	}
	for id, want := range map[string]string{
		"gt-p1new": "waiting for a seat #1 of 3",
		"gt-early": "waiting for a seat #2 of 3",
		"gt-late1": "waiting for a seat #3 of 3",
	} {
		if l, ok := leafSaying(tags[id], "tag", "waiting for a seat"); !ok || l.Text != want {
			t.Errorf("%s wears %+v, want the tag %q", id, l, want)
		}
	}
	l, ok := leafSaying(tags["gt-skpv2"], "tag warnc", "skipped: ")
	if !ok || l.Text != "skipped: not eligible (assigned to gastown/polecats/slit)" {
		t.Errorf("gt-skpv2 wears %+v, want a warning tag with the reason", l)
	}
	if _, ok := leafSaying(tags["gt-skpv2"], "qsub", "not eligible"); !ok {
		t.Errorf("gt-skpv2's card body lost the full reason line: %+v", tags["gt-skpv2"].Leaves)
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

	rows := queueRows(t, dom)
	if len(rows) != 1 {
		t.Fatalf("the ready to dispatch view draws %d rows, want the one bead: %+v", len(rows), rows)
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
	// The tag says what the bead is, so the row need not be read twice.
	for _, l := range rows[0].Leaves {
		if l.Class == "tag warnc" && l.Text == "skipped: overlap hold: gt-okckk is in progress" {
			return
		}
	}
	t.Errorf("the skipped row carries no warning tag with the reason; its cells are %+v", rows[0].Leaves)
}

// A reason is text another agent wrote, so the page writes it as text: markup
// in a reason reads as the characters it is, never as an element (gt-e2cif).
func TestQueueSkipPageReasonIsTextNotMarkup(t *testing.T) {
	t.Parallel()

	const reason = `<b>held</b> & "later"`
	dom := drawQueuePage(t, queueRead(
		skippedRow("gt-evilv", "2026-10-29T11:58:00Z", reason, 1),
	))

	rows := queueRows(t, dom)
	if len(rows) != 1 {
		t.Fatalf("the ready to dispatch view draws %d rows, want the one bead: %+v", len(rows), rows)
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

	// The chip counts what the rows below it list: a row shaped "skipped" lints
	// clean, so a stale page still counts it as ready (2 here) and only stops
	// claiming why it was passed over.
	if got := chipNamed(t, queueChips(t, dom), "ready to dispatch"); got.Count != "2" {
		t.Errorf("the stale page's ready to dispatch chip counts %s, want 2 (the shaped bead and the skipped one)", got.Count)
	}

	// No row claims a skip either: the row the reading gave shape "skipped" is
	// only "ready", with neither the skip tag nor the reason line.
	rows := queueRows(t, dom)
	if len(rows) != 2 {
		t.Fatalf("the stale ledger draws %d rows, want both beads of the reading: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if strings.Contains(r.Text, "overlap hold") || strings.Contains(r.Text, "skipped since") {
			t.Errorf("a row claims a skip the stale ledger cannot back: %q", r.Text)
		}
		for _, l := range r.Leaves {
			if strings.HasPrefix(l.Text, "skipped") || strings.HasPrefix(l.Text, "waiting for a seat") {
				t.Errorf("a row wears the skipped tag while the ledger is stale: %q", r.Text)
			}
		}
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

// A bead with a missing or non-numeric priority takes the last place in the
// seat order rather than breaking the sort with NaN (gt-qt0ym).
func TestQueueSkipPageSeatPlaceMissingPriority(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		priority any
	}{
		{"missing", nil},
		{"non-numeric", "high"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			odd := skippedRow("gt-nopri", "2026-10-29T11:58:00Z", "no seat: over the town total", 4)
			odd["created_at"] = "2026-10-27T10:00:00Z"
			if tt.priority == nil {
				delete(odd, "priority")
			} else {
				odd["priority"] = tt.priority
			}
			normal := skippedRow("gt-normal", "2026-10-29T11:58:00Z", "no seat: over the town total", 4)
			normal["created_at"] = "2026-10-29T10:00:00Z"
			dom := drawQueuePage(t, queueRead(odd, normal))

			want := map[string]string{"gt-normal": "waiting for a seat #1 of 2", "gt-nopri": "waiting for a seat #2 of 2"}
			for _, r := range queueRows(t, dom) {
				for id, tag := range want {
					if !strings.Contains(r.Text, id) {
						continue
					}
					if l, ok := leafSaying(r, "tag", "waiting for a seat"); !ok || l.Text != tag {
						t.Errorf("%s wears %+v, want %q", id, l, tag)
					}
				}
			}
		})
	}
}

// TestQueueSkipPageSeatPlaceCapped: the "of M" in a waiting row's tag is the
// count of waiting beads the page holds, so when the server cut the ready list
// (ready_total larger than the rows sent) it reads "of M+" and the title says
// the list was cut; an uncapped list keeps the plain "of M". A reading that
// carries no ready_total cannot show the list was not cut, so it reads "of M+"
// too.
func TestQueueSkipPageSeatPlaceCapped(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		readyTotal any
		wantTag    string
		wantCut    bool
	}{
		{"every ready bead is on the page", 2, "waiting for a seat #1 of 2", false},
		{"the list was cut by the server", 57, "waiting for a seat #1 of 2+", true},
		{"the reading carries no ready_total", nil, "waiting for a seat #1 of 2+", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			first := skippedRow("gt-first", "2026-10-29T11:58:00Z", "no seat: over the town total", 4)
			first["created_at"] = "2026-10-27T10:00:00Z"
			second := skippedRow("gt-second", "2026-10-29T11:58:00Z", "no seat: over the town total", 4)
			second["created_at"] = "2026-10-28T10:00:00Z"
			queue := queueRead(first, second)
			if tt.readyTotal == nil {
				delete(queue, "ready_total")
			} else {
				queue["ready_total"] = tt.readyTotal
			}
			dom := drawQueuePage(t, queue)

			var found bool
			for _, r := range queueRows(t, dom) {
				if !strings.Contains(r.Text, "gt-first") {
					continue
				}
				l, ok := leafSaying(r, "tag", "waiting for a seat")
				if !ok || l.Text != tt.wantTag {
					t.Fatalf("gt-first wears %+v, want %q", l, tt.wantTag)
				}
				if cut := strings.Contains(l.Title, "list was cut"); cut != tt.wantCut {
					t.Errorf("tag title %q: says the list was cut = %v, want %v", l.Title, cut, tt.wantCut)
				}
				found = true
			}
			if !found {
				t.Fatal("the page drew no row for gt-first")
			}
		})
	}
}
