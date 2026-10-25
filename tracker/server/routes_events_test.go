package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// eventsStub answers EventsAfter; any other method panics on the nil embedded
// interface, which a test shows as a failure.
type eventsStub struct {
	store.Store
	after func(ctx context.Context, after int64, limit int) (api.EventsPage, error)
}

func (s eventsStub) EventsAfter(ctx context.Context, a int64, l int) (api.EventsPage, error) {
	return s.after(ctx, a, l)
}

func eventsServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	s := New(Config{Auth: fakeAuth{"good-token": "town/crew/dipper"}, Health: fakeHealth{}, SchemaLevel: 3, Store: st})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func eventsGet(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer good-token")
	req.Header.Set(api.SchemaHeader, "3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Unit tier: httptest, stub store, no Postgres; milliseconds per case.
func TestEventsParsesAfterAndLimit(t *testing.T) {
	t.Parallel()
	type got struct {
		after int64
		limit int
	}
	var asked []got
	ts := eventsServer(t, eventsStub{after: func(_ context.Context, a int64, l int) (api.EventsPage, error) {
		asked = append(asked, got{a, l})
		return api.EventsPage{NextAfter: a}, nil
	}})
	for _, path := range []string{"/v1/events", "/v1/events?after=42", "/v1/events?after=7&limit=25", "/v1/events?limit=3", "/v1/events?after=9223372036854775807"} {
		if code, body := eventsGet(t, ts.URL+path); code != 200 {
			t.Errorf("GET %s = %d %q, want 200", path, code, body)
		}
	}
	want := []got{{0, 0}, {42, 0}, {7, 25}, {0, 3}, {9223372036854775807, 0}}
	if len(asked) != len(want) {
		t.Fatalf("store asked %v, want %v", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Errorf("request %d reached the store as %+v, want %+v", i, asked[i], want[i])
		}
	}
}

func TestEventsRefusesNegativeNonNumericAndOverflowingValues(t *testing.T) {
	t.Parallel()
	ts := eventsServer(t, eventsStub{after: func(context.Context, int64, int) (api.EventsPage, error) {
		t.Error("the store must not be called with an invalid query")
		return api.EventsPage{}, nil
	}})
	for _, path := range []string{
		"/v1/events?after=-1", "/v1/events?after=abc", "/v1/events?after=1.5", "/v1/events?after=9223372036854775808",
		"/v1/events?limit=-1", "/v1/events?limit=x", "/v1/events?limit=2147483648",
	} {
		code, body := eventsGet(t, ts.URL+path)
		if e := decodeErr(t, body); code != 400 || e.Kind != api.KindInvalid {
			t.Errorf("GET %s = %d %q, want 400 invalid", path, code, body)
		}
	}
}

func TestEventsAnswersAnEmptyFeedAsAnArrayKeepingNextAfterAndFirstSeq(t *testing.T) {
	t.Parallel()
	ts := eventsServer(t, eventsStub{after: func(_ context.Context, a int64, _ int) (api.EventsPage, error) {
		return api.EventsPage{NextAfter: a, FirstSeq: 17}, nil // a nil Events slice from the store
	}})
	code, body := eventsGet(t, ts.URL+"/v1/events?after=40")
	if code != 200 || !strings.Contains(body, `"events":[]`) || !strings.Contains(body, `"next_after":40`) || !strings.Contains(body, `"first_seq":17`) {
		t.Errorf("empty feed = %d %q, want [] and next_after equal to the after sent, first_seq carried", code, body)
	}
}

func TestEventsPassesStoreErrorsAndHidesInternalOnes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   api.Kind
		leak   string
	}{
		"invalid":  {api.Invalid("limit too large"), 400, api.KindInvalid, ""},
		"internal": {context.DeadlineExceeded, 503, api.KindUnavailable, "deadline"},
	} {
		ts := eventsServer(t, eventsStub{after: func(context.Context, int64, int) (api.EventsPage, error) { return api.EventsPage{}, tc.err }})
		code, body := eventsGet(t, ts.URL+"/v1/events")
		if e := decodeErr(t, body); code != tc.status || e.Kind != tc.kind {
			t.Errorf("%s: %d %q, want %d %s", name, code, body, tc.status, tc.kind)
		}
		if tc.leak != "" && strings.Contains(body, tc.leak) {
			t.Errorf("%s: the response leaks the cause: %q", name, body)
		}
	}
}

func TestEventsNeedsAToken(t *testing.T) {
	t.Parallel()
	ts := eventsServer(t, eventsStub{})
	code, _ := get(t, ts.URL+"/v1/events", map[string]string{api.SchemaHeader: "3"})
	if code != 401 {
		t.Errorf("GET /v1/events without a token = %d, want 401", code)
	}
}
