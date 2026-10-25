package client

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// Unit tier: canned httptest responses; loopback only, no sleeps.
func TestEventsSendsAfterAndLimitAndFillsNil(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var queries []string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"events":null,"next_after":12,"first_seq":4}`))
	}), nil)
	page, err := c.Events(t.Context(), 12, 0)
	if err != nil || page.Events == nil || len(page.Events) != 0 || page.NextAfter != 12 || page.FirstSeq != 4 {
		t.Errorf("Events = %+v, %v; want an empty non-nil slice with next_after and first_seq carried", page, err)
	}
	if _, err := c.Events(t.Context(), 5, 30); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 || queries[0] != "after=12" || queries[1] != "after=5&limit=30" {
		t.Errorf("queries = %v, want after=12 then after=5&limit=30 (limit only when set)", queries)
	}
}

func TestEventsAllFollowsThreePagesAndStopsOnAnEmptyOne(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var afters []string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		mu.Lock()
		afters = append(afters, r.URL.Query().Get("after"))
		mu.Unlock()
		page := api.EventsPage{NextAfter: after}
		if after < 6 {
			page.Events = []api.Event{{Seq: after + 1}, {Seq: after + 2}}
			page.NextAfter = after + 2
		}
		_ = json.NewEncoder(w).Encode(page)
	}), nil)
	got, err := c.EventsAll(t.Context(), 0)
	if err != nil || len(got) != 6 || got[5].Seq != 6 {
		t.Fatalf("EventsAll = %d events, %v; want 6", len(got), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(afters) != 4 || afters[0] != "0" || afters[3] != "6" {
		t.Errorf("afters asked = %v, want 0, 2, 4, then 6 (the empty page that ends it)", afters)
	}
}

func TestEventsAllRefusesAServerThatNeverAdvances(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	calls := 0
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n > 3 {
			// A client that tolerates the stuck cursor would loop here forever;
			// failing the call ends the test with calls > 1 instead of a hang.
			writeErr(w, 500, api.Invalid("test server: the client kept following a stuck cursor"))
			return
		}
		_, _ = w.Write([]byte(`{"events":[{"seq":1}],"next_after":0,"first_seq":1}`))
	}), nil)
	got, err := c.EventsAll(t.Context(), 0)
	if err == nil || got != nil {
		t.Errorf("EventsAll = %v, %v; want an error and no partial list", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("server was called %d times, want 1: the stuck cursor must stop the loop at once", calls)
	}
}
