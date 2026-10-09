package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// questionPageNow is the instant the page fixtures below are written against.
var questionPageNow = time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)

// The Questions pane is read where the operator looks first: the top of the
// left column, above Escalations, which is what the layout test pins. What it
// draws is the state the reader decided — an unreadable read is named rather
// than drawn as an empty pane, and the rows are drawn as cards.
func TestQuestionsPanelDrawsTheStateTheReaderDecided(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderQuestions")
	for _, want := range []string{
		`const q = s.questions;`,
		`box.append(el("div", "badc", "unavailable: questions could not be read"))`,
		`const rows = q.rows || [];`,
		`if (!rows.length) { box.append(el("div", "empty", "none open")); return; }`,
		`for (const r of rows) box.append(questionCard(r));`,
		`if (q.more) box.append(el("div", "empty", "+" + q.more + " more"));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderQuestions has no %q", want)
		}
	}
	// The question is the overseer's own text, so every cell goes through el(),
	// which writes textContent: a tag in it shows as that text, never markup.
	if strings.Contains(draw, "innerHTML") {
		t.Error("renderQuestions sets markup from the question's text")
	}

	all := pageFunc(t, "renderAll")
	if !strings.Contains(all, "renderQuestions(state)") {
		t.Error("renderAll does not draw the Questions panel")
	}
	if !strings.Contains(string(indexHTML), `<section><h2>Questions <span class="r" id="questionsnote"></span></h2><div class="body" id="questions"></div></section>`) {
		t.Error("the Questions section is not the markup its neighbours are")
	}
}

// The header names the count, and past four hours the amber the page marks
// every stale reading with: the question that has waited longest is the one the
// operator has to see.
func TestQuestionsPanelMarksTheOldestPastFourHours(t *testing.T) {
	t.Parallel()

	if !strings.Contains(string(indexHTML), "const QUESTION_OLDEST_SEC = 4 * 3600;") {
		t.Error("the page does not hold the four-hour threshold")
	}
	draw := pageFunc(t, "renderQuestions")
	for _, want := range []string{
		`const oldest = ages.length ? Math.max(...ages) : null;`,
		`const stale = oldest != null && oldest > QUESTION_OLDEST_SEC;`,
		`note.className = stale ? "r warnc" : "r";`,
		`(stale ? " · oldest " + fmtDur(oldest) : "")`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderQuestions has no %q", want)
		}
	}
}

// One question is one card: what the overseer asks, the answer it recommends,
// the work it holds up, whether Sloan has answered it, and the line that
// answers it. The page writes nothing — that line is the whole answer path, and
// the operator copies it into a terminal.
func TestQuestionCardCarriesTheAnswerPath(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "questionCard")
	for _, want := range []string{
		`head.append(el("span", "bead", r.id));`,
		`if (r.question) c.append(el("div", "qtext", r.question));`,
		`if (r.recommended) c.append(el("div", "qrec", "Recommended: " + r.recommended));`,
		`if ((r.held || []).length) c.append(el("div", "qheld", "Held: " + r.held.join(", ")));`,
		`el("span", "qstate", r.state === "answered" ? "answered, overseer acting" : "awaiting you")`,
		`c.append(el("div", "qcmd", 'bd comments add ' + r.id + ' "<answer>"'));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("questionCard has no %q", want)
		}
	}
	if strings.Contains(draw, "innerHTML") {
		t.Error("questionCard sets markup from the question's text")
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
				{ID: "gt-cr62m", Title: "retry policy", Question: "retry?", State: QuestionAwaiting, Comments: 1},
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

	st := h.State()
	if st.Health.Verdict != "green" {
		t.Errorf("open questions moved the health verdict to %q", st.Health.Verdict)
	}
	if st.Escalations == nil || len(st.Escalations.Rows) != 1 {
		t.Errorf("open questions changed the escalations the pane holds: %+v", st.Escalations)
	}
}
