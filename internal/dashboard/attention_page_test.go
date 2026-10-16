package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// The panel's rows and its note are drawn by index.html's own script, so the
// tests below run that script over the reading the server would send
// (page_harness_test.go) rather than matching its source: a render function
// that mentions the right names draws nothing (gt-tq4ee).

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

	v, err := dom.vm.RunString(`JSON.stringify(__byId["attention"].children.map(c => ({
		text: c.textContent, owner: c.dataset.owner || "", hidden: !!c.hidden,
		cells: Array.from(c.children).map(k => k.tagName.toLowerCase() + " " + k.className)})))`)
	if err != nil {
		t.Fatalf("reading the panel's rows: %v", err)
	}
	var out []panelDrawn
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the panel's rows: %v", err)
	}
	return out
}

// panelCells reports whether one row drew a cell of the given tag and class.
func panelCells(row panelDrawn, cell string) bool {
	for _, c := range row.Cells {
		if c == cell {
			return true
		}
	}
	return false
}

// attentionState is the reading the panel is drawn from: the rows of the one
// list, and what the readers behind it had to say.
func attentionState(at map[string]any) map[string]any {
	return map[string]any{"attention": at}
}

// One row per thing waiting, each tagged with its owner and carrying what
// answering it takes: the question's own words and the line that answers it,
// the escalation's severity and raiser, the queue item's rule and level
// (gt-bi6w0).
func TestThePanelDrawsTheThreeKindsOfRow(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{
		map[string]any{"kind": "question", "owner": "sloan", "id": "gt-q1", "title": "retry policy",
			"question": "how many times should a push retry?", "recommended": "retry twice",
			"held_count": 2, "held": []any{"gt-h1"}, "effort": "quick",
			"state": "awaiting", "priority": 0, "age_sec": 5400},
		map[string]any{"kind": "escalation", "owner": "overseer", "id": "hq-9", "title": "slot held",
			"severity": "critical", "escalated_by": "gastown/polecats/opal", "age_sec": 600},
		map[string]any{"kind": "item", "owner": "overseer", "id": "slot:gastown", "title": "the slot is held",
			"level": "crit", "rule": "slot.held", "age_sec": 60},
	}}))

	rows := panelRows(t, dom)
	if len(rows) != 3 {
		t.Fatalf("the panel drew %d rows (%+v), want one per kind", len(rows), rows)
	}

	// The question leads: it is the row type that stops the town while it waits,
	// and it is the only one the human has to answer.
	q := rows[0]
	if q.Owner != "sloan" || !panelCells(q, "span tag own-sloan") {
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

// A question the reader could not parse is still a row — a question the
// operator cannot answer from the page is one they should still know about —
// and it says which part of the bead is missing (gt-bi6w0).
func TestThePanelNamesAQuestionItCannotDraw(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{
		map[string]any{"kind": "question", "owner": "sloan", "id": "gt-q2", "title": "a question the bead lost",
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

// Nothing waiting collapses to one line, and a read that failed or has not
// reported is named rather than drawn as that line: "nothing is waiting" and
// "the list could not be read" are different things to tell the operator
// (gt-bi6w0).
func TestThePanelCollapsesWhenNothingIsWaiting(t *testing.T) {
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
			want: []string{"nothing waiting"},
			gone: []string{"have not been read yet", "unavailable"},
		},
		{
			name: "a read that failed is named",
			at:   map[string]any{"rows": []any{}, "failed": []any{"escalations"}},
			want: []string{"unavailable: the escalations could not be read"},
			gone: []string{"nothing waiting"},
		},
		{
			name: "a reader that has not reported is not one that found nothing",
			at:   map[string]any{"rows": []any{}, "pending": []any{"escalations", "questions"}},
			want: []string{"the escalations and the questions have not been read yet"},
			gone: []string{"nothing waiting"},
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

// The note counts what is waiting and names the rows the readers capped out of
// the list, and past four hours it marks how long the oldest question has
// waited: that question is the row type that stops the town (gt-bi6w0).
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
			rows:      []any{map[string]any{"kind": "escalation", "owner": "overseer", "id": "hq-9", "title": "slot held"}},
			more:      2,
			wantNote:  "1 of 3 waiting",
			wantClass: "r",
		},
		{
			name: "a question past four hours is marked",
			rows: []any{map[string]any{"kind": "question", "owner": "sloan", "id": "gt-q1",
				"title": "retry policy", "age_sec": 5 * 3600}},
			wantNote:  "1 waiting · oldest question 5h00m",
			wantClass: "r warnc",
		},
		{
			name:       "a fresh question is not",
			rows:       []any{map[string]any{"kind": "question", "owner": "sloan", "id": "gt-q1", "title": "retry policy", "age_sec": 600}},
			wantNote:   "1 waiting",
			wantClass:  "r",
			wantAbsent: "oldest question",
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

// The owner tag is also the panel's one filter: a chip per owner carrying the
// count of the rows it is showing, on by default, and a chip switched off hides
// the rows that owner has to act on (gt-bi6w0).
func TestTheOwnerChipFiltersTheRows(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(map[string]any{"rows": []any{
		map[string]any{"kind": "question", "owner": "sloan", "id": "gt-q1", "title": "retry policy"},
		map[string]any{"kind": "escalation", "owner": "overseer", "id": "hq-9", "title": "slot held"},
		map[string]any{"kind": "item", "owner": "overseer", "id": "slot:gastown", "title": "the slot is held"},
	}}))

	// One chip per owner the reader can tag a row with, each naming its owner
	// and carrying its own count: the reader of either role sees the whole list
	// and filters it.
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
	if len(chips) != 2 || !strings.Contains(chips[0].Title, "sloan") || chips[0].Count != "1" ||
		!strings.Contains(chips[1].Title, "overseer") || chips[1].Count != "2" {
		t.Errorf("the chips read %+v, want the human's one row and the overseer's two", chips)
	}

	for _, r := range panelRows(t, dom) {
		if r.Hidden {
			t.Errorf("row %+v is hidden before a chip was switched off", r)
		}
	}

	// The overseer's chip switched off hides its rows and leaves the human's.
	if _, err := dom.vm.RunString(`__byId["attentionchips"].children[1].onclick();`); err != nil {
		t.Fatalf("switching the overseer's chip off: %v", err)
	}
	for _, r := range panelRows(t, dom) {
		want := r.Owner == "overseer"
		if r.Hidden != want {
			t.Errorf("row %+v hidden is %v with the overseer's chip off, want %v", r, r.Hidden, want)
		}
	}
}

// The strip's "needs you" is the human's share of the same payload the panel
// draws, and a count taken beside rows the readers capped out is a floor: the
// strip draws it as one rather than as a whole that quietly leaves them out
// (gt-bi6w0).
func TestTheStripDrawsTheHumanCountAsAFloor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		at   map[string]any
		want string
	}{
		{
			name: "every row was read",
			at:   map[string]any{"mine": 2, "rows": []any{map[string]any{"owner": "sloan"}}},
			want: "2 needs you",
		},
		{
			name: "a reader capped rows out of the list",
			at:   map[string]any{"mine": 2, "more": 3, "rows": []any{map[string]any{"owner": "sloan"}}},
			want: "2+ needs you",
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
	if item == nil || item.Rule != attention.RuleHostDiskCrit || item.Owner != string(attention.OwnerSloan) {
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
