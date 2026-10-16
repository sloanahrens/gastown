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

// The panel that draws these rows — its note, its rows and its owner filter —
// is exercised over the page's own script in attention_page_test.go, which runs
// renderAttention rather than matching its source (gt-tq4ee).

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
