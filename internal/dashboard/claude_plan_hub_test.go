package dashboard

import (
	"fmt"
	"math"
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

// A price the operator set to something that is not a number is no price: the
// meter falls back to the default, and /api/state still encodes, since a NaN
// or an infinity is dollars the encoder refuses to write and would take the
// whole state down with the one field.
func TestClaudePlanSurvivesAPriceThatIsNotANumber(t *testing.T) {
	t.Parallel()

	path := claudePlanFile(t, planLine(claudePlanNow, 8, 38, claudePlanNow.Add(48*time.Hour).Unix()))
	for _, price := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		c := NewClaudePlanReader(path, price).Read(claudePlanNow)
		if c.PlanUSDMonth != ClaudePlanDefaultUSD {
			t.Errorf("a price of %v priced the plan at %v, want the default %v",
				price, c.PlanUSDMonth, ClaudePlanDefaultUSD)
		}

		h := NewHub(Config{ClaudePlan: func() *ClaudePlan { return c }})
		h.pollClaudePlan()
		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("a price of %v made GET /api/state = %d: %s", price, rec.Code, rec.Body.String())
		}
		if want := fmt.Sprintf(`"plan_usd_month":%d`, ClaudePlanDefaultUSD); !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the state served for a price of %v does not carry %s: %s", price, want, rec.Body.String())
		}
	}
}
