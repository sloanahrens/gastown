package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// One panel replaced three: the page holds one section for what is waiting on a
// decision, and the Questions, Escalations and Alerts sections are gone from it
// (gt-bi6w0).
func TestOnePanelReplacedTheThree(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	if !strings.Contains(page, `id="attention"`) {
		t.Fatal("index.html has no Attention panel")
	}
	for _, gone := range []string{`id="questions"`, `id="escalations"`, `id="alerts"`, `renderQuestions(`, `renderEscalations(`, `renderAlerts(`} {
		if strings.Contains(page, gone) {
			t.Errorf("index.html still holds %s, which the one panel replaced", gone)
		}
	}
}

// The owner tag is also the panel's one filter: a chip per owner, carrying the
// count of the rows it is showing, and a row starts hidden when its owner's
// chip is off. The choice is the reader's, kept where the feed's chips are.
func TestTheOwnerTagIsThePanelsFilter(t *testing.T) {
	t.Parallel()

	chips := pageFunc(t, "renderOwnerChips")
	for _, want := range []string{
		`for (const r of rows) counts[r.owner] = (counts[r.owner] || 0) + 1;`,
		`const b = el("button", "chip" + (shownOwners.has(o) ? " on" : ""), o);`,
		`b.append(el("b", "", String(counts[o] || 0)));`,
		`localStorage.setItem("gt-attention-owners", JSON.stringify([...shownOwners]))`,
		`applyOwnerFilter();`,
	} {
		if !strings.Contains(chips, want) {
			t.Errorf("renderOwnerChips has no %q", want)
		}
	}
	if !strings.Contains(pageFunc(t, "applyOwnerFilter"), `r.hidden = !shownOwners.has(r.dataset.owner)`) {
		t.Error("applyOwnerFilter does not hide a row whose owner's chip is off")
	}
	// A row drawn before the filter is shown or hidden by its own owner, so a
	// reload does not draw every owner's rows behind a chip that is off.
	if !strings.Contains(pageFunc(t, "attentionRow"), `row.hidden = !shownOwners.has(r.owner);`) {
		t.Error("attentionRow draws a row the owner filter has switched off")
	}
	if !strings.Contains(string(indexHTML), `const shownOwners = new Set(JSON.parse(localStorage.getItem("gt-attention-owners") || "null") || OWNERS);`) {
		t.Error("the owner filter is not kept in the reader's own storage")
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
				{ID: "gt-q1", Title: "retry policy", Priority: 0, Recommended: "retry twice", State: QuestionAwaiting},
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

	// The cleared line takes the row off the list, and the two beads stay.
	writeAttentionEvents(t, town, clearedEvent(wakeEvent(attention.RuleHostDiskCrit, "host:disk", "the volume is 96% full", attention.SeverityHigh)))
	h.pollAttention()
	if st := h.State(); len(st.Attention.Rows) != 2 {
		t.Errorf("the list holds %+v after the clear, want the two beads", st.Attention.Rows)
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
