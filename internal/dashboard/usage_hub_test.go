package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The state the page reads carries the panel, and a read that failed reaches it
// as itself: /api/state serves the unavailable flag, not an empty day.
func TestUsageReachesTheAPI(t *testing.T) {
	t.Parallel()

	reads := []*Usage{
		{Window: "24h", TopBeads: []UsageTopBead{{Bead: "gt-a", Uncached: 120}}},
		{Unavailable: true},
	}
	for _, u := range reads {
		h := NewHub(Config{
			Now:   func() time.Time { return usageNow },
			Usage: func() *Usage { return u },
		})
		h.pollUsage()

		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/state = %d", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"usage"`) {
			t.Fatalf("/api/state does not carry the usage the hub holds: %s", body)
		}
		if got := strings.Contains(body, `"unavailable":true`); got != u.Unavailable {
			t.Errorf("/api/state unavailable = %v, want %v: %s", got, u.Unavailable, body)
		}
		if st := h.State(); st.Usage != u {
			t.Errorf("the hub holds %+v, want the reader's %+v", st.Usage, u)
		}
	}
}

// A reader that reports nothing leaves the state as it was; an unconfigured hub
// has no panel at all.
func TestUsageNilReadKeepsTheLastState(t *testing.T) {
	t.Parallel()

	var next *Usage
	h := NewHub(Config{Usage: func() *Usage { return next }})
	h.pollUsage()
	if h.State().Usage != nil {
		t.Fatal("a nil read set the panel")
	}
	next = &Usage{Window: "24h"}
	h.pollUsage()
	next = nil
	h.pollUsage()
	if got := h.State().Usage; got == nil || got.Window != "24h" {
		t.Errorf("a nil read replaced the last state with %+v", got)
	}
}

// The poll defaults to a minute.
func TestUsageEveryDefaultsToAMinute(t *testing.T) {
	t.Parallel()

	var c Config
	c.defaults()
	if c.UsageEvery != 60*time.Second {
		t.Errorf("UsageEvery defaults to %v, want 1m", c.UsageEvery)
	}
}
