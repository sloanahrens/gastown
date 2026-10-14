package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var deepReviewPageNow = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

// The Deep review pane draws what the reader decided: an unreadable list is
// named rather than drawn as "no runs", a hold replaces the schedules' due
// times, the standing is one cell per schedule, and the rows are drawn cell by
// cell through el(), which writes textContent.
func TestDeepReviewPanelDrawsTheStateTheReaderDecided(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderDeepReview")
	for _, want := range []string{
		`const d = s.deepreview;`,
		`box.append(el("div", "badc", "unavailable: deep reviews could not be read"))`,
		`const nexts = d.next || [];`,
		`cell.append(el("b", "warnc", "paused"), el("span", "", d.paused))`,
		`cell.append(el("b", "", n.text), el("span", "", n.name || "deep review"))`,
		`const rows = d.rows || [];`,
		`box.append(el("div", "empty", "no runs yet"))`,
		`el("span", "badge warnc", "unparsed")`,
		`el("td", "title", r.kind || "–")`,
		`r.duration_sec == null ? "–" : fmtDur(r.duration_sec)`,
		`el("td", "title", r.digest || "–")`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderDeepReview has no %q", want)
		}
	}
	// A run's id and a hold's reason are text, and a tag in them shows as that
	// text and never as markup.
	if strings.Contains(draw, "innerHTML") {
		t.Error("renderDeepReview sets markup from the reader's text")
	}

	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderDeepReview(state)") {
		t.Error("renderAll does not draw the Deep review panel")
	}
	if !strings.Contains(string(indexHTML), `<section><h2>Deep review <span class="r" id="deepreviewnote"></span></h2><div class="body" id="deepreview"></div></section>`) {
		t.Error("the Deep review section is not the markup its neighbours are")
	}
	// After Questions, before Escalations.
	html := string(indexHTML)
	if q, d, e := strings.Index(html, `id="questions"`), strings.Index(html, `id="deepreview"`), strings.Index(html, `id="escalations"`); !(q < d && d < e) {
		t.Errorf("the Deep review section is not between Questions and Escalations (%d, %d, %d)", q, d, e)
	}
}

// The panel never sets page markup anywhere in its source, whatever its
// function draws.
func TestDeepReviewPanelSetsNoMarkup(t *testing.T) {
	t.Parallel()

	if strings.Contains(pageFunc(t, "renderDeepReview"), "insertAdjacentHTML") {
		t.Error("renderDeepReview inserts markup")
	}
}

// The state the page reads carries the panel, and a read that failed reaches it
// as itself: /api/state serves the unavailable flag, not an empty list.
func TestDeepReviewReachesTheAPI(t *testing.T) {
	t.Parallel()

	reads := []*DeepReview{
		{Next: []DeepReviewNext{{Name: "deep-review-slice", Text: "in 3h 20m"}}, Rows: []DeepReviewRow{{ID: "gt-run1", Kind: DeepReviewKindSlice, Status: DeepReviewClosed, Findings: 2, Filed: 1}}},
		{Unavailable: true},
	}
	for _, d := range reads {
		h := NewHub(Config{
			Now:        func() time.Time { return deepReviewPageNow },
			DeepReview: func() *DeepReview { return d },
		})
		h.pollDeepReview()

		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/state = %d", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"deepreview"`) {
			t.Fatalf("/api/state does not carry the deep review the hub holds: %s", body)
		}
		if got := strings.Contains(body, `"unavailable":true`); got != d.Unavailable {
			t.Errorf("/api/state unavailable = %v, want %v: %s", got, d.Unavailable, body)
		}
		if st := h.State(); st.DeepReview != d {
			t.Errorf("the hub holds %+v, want the reader's %+v", st.DeepReview, d)
		}
	}
}

// A reader that reports nothing leaves the state as it was; an unconfigured hub
// has no panel at all.
func TestDeepReviewNilReadKeepsTheLastState(t *testing.T) {
	t.Parallel()

	var next *DeepReview
	h := NewHub(Config{DeepReview: func() *DeepReview { return next }})
	h.pollDeepReview()
	if h.State().DeepReview != nil {
		t.Fatal("a nil read set the panel")
	}
	next = &DeepReview{Next: []DeepReviewNext{{Name: "deep-review-slice", Text: "due now"}}}
	h.pollDeepReview()
	next = nil
	h.pollDeepReview()
	if got := h.State().DeepReview; got == nil || len(got.Next) != 1 || got.Next[0].Text != "due now" {
		t.Errorf("a nil read replaced the last state with %+v", got)
	}
}

// The poll defaults to a minute.
func TestDeepReviewEveryDefaultsToAMinute(t *testing.T) {
	t.Parallel()

	var c Config
	c.defaults()
	if c.DeepReviewEvery != 60*time.Second {
		t.Errorf("DeepReviewEvery defaults to %v, want 1m", c.DeepReviewEvery)
	}
}
