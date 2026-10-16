package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// questionPageNow is the instant the page fixtures below are written against.
var questionPageNow = time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)

// The panel is read where the operator looks first — the top of the left
// column, which is what the layout test pins — and what it draws is the state
// the reader decided: a read that failed is named rather than drawn as an empty
// panel, and a reader that has not reported is not a reader that found nothing.
func TestAttentionPanelDrawsTheStateTheReaderDecided(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderAttention")
	for _, want := range []string{
		`const at = s.attention;`,
		`const rows = at.rows || [];`,
		`if (failed.length) box.append(el("div", "badc", "unavailable: the " + failed.join(" and the ") + " could not be read"));`,
		`if (!rows.length) {`,
		`box.append(el("div", "empty", "nothing needs you"));`,
		`if (pending.length) box.append(el("div", "empty", "the " + pending.join(" and the ") + " have not been read yet"));`,
		`for (const r of rows) box.append(attentionRow(r));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderAttention has no %q", want)
		}
	}
	// The note is what says the readers capped rows out of the list, so the
	// panel draws no second count of them.
	if !strings.Contains(draw, `rows.length + (at.more ? " of " + (rows.length + at.more) : "") + " waiting"`) {
		t.Error("renderAttention does not count the rows its readers capped out")
	}
	// A question, a recommendation and a queue summary are other agents' own
	// text, so every cell goes through el(), which writes textContent: a tag in
	// them shows as that text, never markup.
	if strings.Contains(draw, "innerHTML") {
		t.Error("renderAttention sets markup from the row's text")
	}

	all := pageFunc(t, "renderAll")
	if n := strings.Count(all, "renderAttention(state)"); n != 1 {
		t.Errorf("renderAll draws the Attention panel %d times, want 1", n)
	}
	if !strings.Contains(string(indexHTML), `<section><h2>Needs you <span class="chips" id="attentionchips"></span> <span class="r" id="attentionnote"></span></h2><div class="body" id="attention"></div></section>`) {
		t.Error("the Needs you section is not the markup its neighbours are")
	}
}

// The header names what is waiting and, past four hours, the amber the page
// marks every stale reading with: the question is the row type that stops the
// town until it is answered, so the one that has waited longest is the one the
// operator has to see.
func TestAttentionPanelMarksTheOldestQuestionPastFourHours(t *testing.T) {
	t.Parallel()

	if !strings.Contains(string(indexHTML), "const QUESTION_OLDEST_SEC = 4 * 3600;") {
		t.Error("the page does not hold the four-hour threshold")
	}
	draw := pageFunc(t, "renderAttention")
	for _, want := range []string{
		`const asks = rows.filter(r => r.kind === "question").map(r => r.age_sec).filter(a => a != null);`,
		`const oldest = asks.length ? Math.max(...asks) : null;`,
		`const stale = oldest != null && oldest > QUESTION_OLDEST_SEC;`,
		`note.className = stale ? "r warnc" : "r";`,
		`(stale ? " · oldest question " + fmtDur(oldest) : "")`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderAttention has no %q", want)
		}
	}
}

// One row: what it is about, its owner, its age and, for a question, what
// answering it holds up and the line that answers it. The page writes nothing —
// that line is the whole answer path, and the operator copies it into a
// terminal.
func TestAttentionRowCarriesTheOwnerAndTheAnswerPath(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "attentionRow")
	// The kinds the row branches on are the reader's own, so a kind renamed on
	// one side is a failure rather than a row drawn as another kind.
	for _, kind := range []string{AttentionQuestion, AttentionEscalation, AttentionItem} {
		if !strings.Contains(draw, `r.kind === "`+kind+`"`) {
			t.Errorf("attentionRow does not draw a %s row", kind)
		}
	}
	for _, want := range []string{
		`row.dataset.owner = r.owner;`,
		`row.append(el("span", "tag own-" + r.owner, r.owner || "?"));`,
		`row.append(el("span", "bead", r.id || ""));`,
		`row.append(el("span", r.priority <= 1 ? "qpri warnc" : "qpri", "P" + r.priority));`,
		`if (r.effort) row.append(el("span", "qeffort", r.effort));`,
		`if (r.recommended) row.append(el("div", "qrec", "Recommended: " + r.recommended));`,
		`held.push("holds " + r.held_count + (r.held_count === 1 ? " bead" : " beads"));`,
		`el("span", "qstate", r.state === "answered" ? "answered, overseer acting" : "awaiting you")`,
		`row.append(el("div", "qcmd", 'gt answer ' + r.id + ' "<answer>"'));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("attentionRow has no %q", want)
		}
	}
	if strings.Contains(draw, "innerHTML") {
		t.Error("attentionRow sets markup from the row's text")
	}
	// The copy-ready line is one line the operator can select whole.
	if !strings.Contains(string(indexHTML), `.qcmd{`) {
		t.Error("index.html has no style for the copy-ready line")
	}
}

// The state the page reads carries the panel, and open questions change nothing
// else it draws: /api/state serves them, and the health verdict and the
// escalations count are the readers' own answers whatever the queue holds.
func TestQuestionsReachTheAPIAndChangeNothingElse(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{
		Now: func() time.Time { return questionPageNow },
		Health: func() Health {
			return Health{Line: "all green", Verdict: "green", ReadAt: questionPageNow}
		},
		Escalation: func() *Escalations {
			return &Escalations{Rows: []EscalationRow{{ID: "hq-esc1", Title: "an escalation", Severity: "high"}}}
		},
		Questions: func() *Questions {
			return &Questions{Rows: []QuestionRow{
				{ID: "gt-cr62m", Title: "retry policy", Priority: 1, Question: "retry?",
					HeldCount: 2, Effort: "quick", State: QuestionAwaiting, Comments: 1},
			}}
		},
	})
	h.pollHealth()
	h.pollEscalation()
	h.pollQuestions()

	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"questions"`) || !strings.Contains(body, `"retry policy"`) {
		t.Errorf("/api/state does not carry the questions the hub holds: %s", body)
	}
	// The row carries what the panel orders it by, so the panel that renders it
	// needs no query of its own.
	var state struct {
		Questions Questions `json:"questions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("/api/state is not the page's JSON: %v", err)
	}
	if len(state.Questions.Rows) != 1 {
		t.Fatalf("the state carries %d question rows, want the one the hub holds", len(state.Questions.Rows))
	}
	if r := state.Questions.Rows[0]; r.Priority != 1 || r.HeldCount != 2 || r.Effort != "quick" {
		t.Errorf("the row carries priority %d, held count %d, effort %q, want P1 holding 2 with the quick tag",
			r.Priority, r.HeldCount, r.Effort)
	}

	st := h.State()
	if st.Health.Verdict != "green" {
		t.Errorf("open questions moved the health verdict to %q", st.Health.Verdict)
	}
	if st.Escalations == nil || len(st.Escalations.Rows) != 1 {
		t.Errorf("open questions changed the escalations the reader holds: %+v", st.Escalations)
	}
}

// The row carries the three fields the queue is ordered and read by in the
// order the operator reads them — the priority, the held count, the effort tag
// — so the payload a panel renders needs no join of its own. The order is the
// struct's, which is what encoding/json writes.
func TestQuestionRowCarriesPriorityHeldCountAndEffortInOrder(t *testing.T) {
	t.Parallel()

	row := reflect.TypeOf(QuestionRow{})
	var got []string
	for i := 0; i < row.NumField(); i++ {
		name, _, _ := strings.Cut(row.Field(i).Tag.Get("json"), ",")
		switch name {
		case "priority", "held_count", "effort":
			got = append(got, name)
		}
	}
	if want := []string{"priority", "held_count", "effort"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the row carries %v, want %v", got, want)
	}
}
