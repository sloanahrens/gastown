package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var deepReviewPageNow = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

// deepReviewState is the reading the Deep review block is drawn from: the
// payload under the panel's state, with nothing else configured.
func deepReviewState(d map[string]any) map[string]any {
	return map[string]any{"deepreview": d}
}

// The state the page reads carries the block, and a read that failed reaches it
// as itself: /api/state serves the unavailable flag, not an empty list.
func TestDeepReviewReachesTheAPI(t *testing.T) {
	t.Parallel()

	reads := []*DeepReview{
		{Schedules: []DeepReviewSchedule{{Name: "deep-review-slice", Next: "in 3h 20m",
			Last: &DeepReviewRun{ID: "gt-run1", Status: DeepReviewClosed, Findings: 2, Filed: 1}}}},
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
// has no payload at all.
func TestDeepReviewNilReadKeepsTheLastState(t *testing.T) {
	t.Parallel()

	var next *DeepReview
	h := NewHub(Config{DeepReview: func() *DeepReview { return next }})
	h.pollDeepReview()
	if h.State().DeepReview != nil {
		t.Fatal("a nil read set the payload")
	}
	next = &DeepReview{Schedules: []DeepReviewSchedule{{Name: "deep-review-slice", Next: "due now"}}}
	h.pollDeepReview()
	next = nil
	h.pollDeepReview()
	if got := h.State().DeepReview; got == nil || len(got.Schedules) != 1 || got.Schedules[0].Next != "due now" {
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
