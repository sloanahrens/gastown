package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// issueStub answers the read methods; any other method panics on the nil
// embedded interface, which a test would show as a failure.
type issueStub struct {
	store.Store
	get   func(ctx context.Context, ids []string) (map[string]api.Issue, error)
	list  func(ctx context.Context, f api.ListFilter) (api.IssuePage, error)
	count func(ctx context.Context, f api.ListFilter) (int, error)
}

func (s issueStub) GetIssues(ctx context.Context, ids []string) (map[string]api.Issue, error) {
	return s.get(ctx, ids)
}

func (s issueStub) ListIssues(ctx context.Context, f api.ListFilter) (api.IssuePage, error) {
	return s.list(ctx, f)
}

func (s issueStub) CountIssues(ctx context.Context, f api.ListFilter) (int, error) {
	return s.count(ctx, f)
}

func stubServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	s := New(Config{Auth: fakeAuth{"good-token": "town/crew/dipper"}, Health: fakeHealth{}, SchemaLevel: 3, Store: st})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func call(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
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
func TestGetIssueReturnsTheIssueOr404(t *testing.T) {
	t.Parallel()
	ts := stubServer(t, issueStub{get: func(_ context.Context, ids []string) (map[string]api.Issue, error) {
		if len(ids) == 1 && ids[0] == "gt-abc" {
			return map[string]api.Issue{"gt-abc": {ID: "gt-abc", Title: "found"}}, nil
		}
		return nil, nil // an empty store answers with a nil map
	}})
	code, body := call(t, "GET", ts.URL+"/v1/issues/gt-abc", "")
	var is api.Issue
	if code != 200 || json.Unmarshal([]byte(body), &is) != nil || is.Title != "found" {
		t.Errorf("GET existing = %d %q, want 200 with the issue", code, body)
	}
	code, body = call(t, "GET", ts.URL+"/v1/issues/gt-nope", "")
	if e := decodeErr(t, body); code != 404 || e.Kind != api.KindNotFound {
		t.Errorf("GET missing = %d %q, want 404 not_found", code, body)
	}
}

func TestShowManyKeepsMissingIdsOutAndNeverReturnsNull(t *testing.T) {
	t.Parallel()
	var asked []string
	ts := stubServer(t, issueStub{get: func(_ context.Context, ids []string) (map[string]api.Issue, error) {
		asked = ids
		return nil, nil
	}})
	code, body := call(t, "POST", ts.URL+"/v1/issues:show-many", `{"ids":["gt-a","gt-b"]}`)
	if code != 200 || !strings.Contains(body, `"issues":{}`) {
		t.Errorf("show-many with nothing found = %d %q, want 200 and an empty object, not null", code, body)
	}
	if len(asked) != 2 || asked[0] != "gt-a" {
		t.Errorf("the store was asked for %v, want both ids in order", asked)
	}
}

func TestListPassesTheFilterAndReturnsAnEmptyArrayNotNull(t *testing.T) {
	t.Parallel()
	var got api.ListFilter
	ts := stubServer(t, issueStub{list: func(_ context.Context, f api.ListFilter) (api.IssuePage, error) {
		got = f
		return api.IssuePage{}, nil
	}})
	code, body := call(t, "POST", ts.URL+"/v1/issues:list", `{"statuses":["open"],"assignee":"me","limit":7,"cursor":"c1"}`)
	if code != 200 || !strings.Contains(body, `"issues":[]`) {
		t.Errorf("list = %d %q, want 200 with an empty array, not null", code, body)
	}
	if len(got.Statuses) != 1 || got.Statuses[0] != "open" || got.Assignee != "me" || got.Limit != 7 || got.Cursor != "c1" {
		t.Errorf("the store got filter %+v, want every field passed through", got)
	}
}

func TestCountPassesTheWholeFilterAndAnswersTheStoreCount(t *testing.T) {
	t.Parallel()
	var got api.ListFilter
	ts := stubServer(t, issueStub{count: func(_ context.Context, f api.ListFilter) (int, error) {
		got = f
		return 7, nil
	}})
	code, body := call(t, "POST", ts.URL+"/v1/issues:count",
		`{"filter":{"ephemeral":true,"statuses":["closed"],"closed_before":"2026-10-30T12:00:00Z"}}`)
	var resp api.CountResponse
	if code != 200 || json.Unmarshal([]byte(body), &resp) != nil || resp.Count != 7 {
		t.Errorf("count = %d %q, want 200 with the store's count of 7", code, body)
	}
	want := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if !got.Ephemeral || len(got.Statuses) != 1 || got.Statuses[0] != "closed" || !got.ClosedBefore.Equal(want) {
		t.Errorf("the store got filter %+v, want every field passed through", got)
	}
}

func TestCountRefusesAFilterThatCarriesAPageAndWritesNothing(t *testing.T) {
	t.Parallel()
	called := false
	ts := stubServer(t, issueStub{count: func(_ context.Context, f api.ListFilter) (int, error) {
		called = true
		return 0, nil
	}})
	for name, body := range map[string]string{
		"a limit":  `{"filter":{"limit":10}}`,
		"a cursor": `{"filter":{"cursor":"c1"}}`,
	} {
		code, resp := call(t, "POST", ts.URL+"/v1/issues:count", body)
		if e := decodeErr(t, resp); code != 400 || e.Kind != api.KindInvalid {
			t.Errorf("%s: %d %q, want 400 invalid", name, code, resp)
		}
	}
	if called {
		t.Error("a count with a page reached the store; the route must refuse it first")
	}
}

func TestCountPassesStoreInvalidThrough400AndHidesInternalErrors(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   api.Kind
		leak   string
	}{
		"invalid filter": {api.Invalid("bad filter"), 400, api.KindInvalid, ""},
		"internal":       {context.DeadlineExceeded, 503, api.KindUnavailable, "deadline"},
	} {
		ts := stubServer(t, issueStub{count: func(context.Context, api.ListFilter) (int, error) { return 0, tc.err }})
		code, body := call(t, "POST", ts.URL+"/v1/issues:count", `{"filter":{}}`)
		if e := decodeErr(t, body); code != tc.status || e.Kind != tc.kind {
			t.Errorf("%s: %d %q, want %d %s", name, code, body, tc.status, tc.kind)
		}
		if tc.leak != "" && strings.Contains(body, tc.leak) {
			t.Errorf("%s: the response leaks the cause: %q", name, body)
		}
	}
}

func TestListPassesStoreInvalidThrough400AndHidesInternalErrors(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   api.Kind
		leak   string
	}{
		"invalid filter": {api.Invalid("bad cursor"), 400, api.KindInvalid, ""},
		"internal":       {context.DeadlineExceeded, 503, api.KindUnavailable, "deadline"},
	} {
		ts := stubServer(t, issueStub{list: func(context.Context, api.ListFilter) (api.IssuePage, error) { return api.IssuePage{}, tc.err }})
		code, body := call(t, "POST", ts.URL+"/v1/issues:list", `{}`)
		if e := decodeErr(t, body); code != tc.status || e.Kind != tc.kind {
			t.Errorf("%s: %d %q, want %d %s", name, code, body, tc.status, tc.kind)
		}
		if tc.leak != "" && strings.Contains(body, tc.leak) {
			t.Errorf("%s: the response leaks the cause: %q", name, body)
		}
	}
}

func TestReadRoutesRefuseBadBodiesAsInvalid(t *testing.T) {
	t.Parallel()
	ts := stubServer(t, issueStub{})
	for _, path := range []string{"/v1/issues:list", "/v1/issues:show-many", "/v1/issues:count"} {
		for name, body := range map[string]string{"empty": "", "unknown field": `{"surprise":1}`, "not json": `{`, "trailing data": `{}{}`} {
			code, resp := call(t, "POST", ts.URL+path, body)
			if e := decodeErr(t, resp); code != 400 || e.Kind != api.KindInvalid {
				t.Errorf("%s %s = %d %q, want 400 invalid", path, name, code, resp)
			}
		}
	}
}

func TestReadRoutesNeedAToken(t *testing.T) {
	t.Parallel()
	ts := stubServer(t, issueStub{})
	code, _ := get(t, ts.URL+"/v1/issues/gt-a", map[string]string{api.SchemaHeader: "3"})
	if code != 401 {
		t.Errorf("GET without a token = %d, want 401", code)
	}
}
