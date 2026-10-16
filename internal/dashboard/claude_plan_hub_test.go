package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The state the page reads carries the plan meter, and a read that failed
// reaches it as itself: /api/state serves the unavailable flag, not a week at
// zero.
func TestClaudePlanReachesTheAPI(t *testing.T) {
	t.Parallel()

	reads := []*ClaudePlan{
		{WeeklyPct: 38, PacePerDay24h: 9.2, PlanUSDMonth: 200},
		{Unavailable: true, Stale: true},
	}
	for _, c := range reads {
		h := NewHub(Config{ClaudePlan: func() *ClaudePlan { return c }})
		h.pollClaudePlan()

		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/state = %d", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"claude_plan"`) {
			t.Fatalf("/api/state does not carry the plan meter the hub holds: %s", body)
		}
		if got := strings.Contains(body, `"unavailable":true`); got != c.Unavailable {
			t.Errorf("/api/state unavailable = %v, want %v: %s", got, c.Unavailable, body)
		}
		if st := h.State(); st.ClaudePlan != c {
			t.Errorf("the hub holds %+v, want the reader's %+v", st.ClaudePlan, c)
		}
	}
}

// A reader that reports nothing leaves the state as it was; an unconfigured hub
// has no plan meter at all.
func TestClaudePlanNilReadKeepsTheLastState(t *testing.T) {
	t.Parallel()

	var next *ClaudePlan
	h := NewHub(Config{ClaudePlan: func() *ClaudePlan { return next }})
	h.pollClaudePlan()
	if h.State().ClaudePlan != nil {
		t.Fatal("a nil read set the meter")
	}
	next = &ClaudePlan{WeeklyPct: 38}
	h.pollClaudePlan()
	next = nil
	h.pollClaudePlan()
	if got := h.State().ClaudePlan; got == nil || got.WeeklyPct != 38 {
		t.Errorf("a nil read replaced the last state with %+v", got)
	}
}

// The poll defaults to a minute, the clock the status line's series moves on.
func TestClaudePlanEveryDefaultsToAMinute(t *testing.T) {
	t.Parallel()

	var c Config
	c.defaults()
	if c.ClaudePlanEvery != 60*time.Second {
		t.Errorf("ClaudePlanEvery defaults to %v, want 1m", c.ClaudePlanEvery)
	}
}
