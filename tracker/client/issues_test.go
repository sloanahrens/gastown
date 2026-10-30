package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

type seen struct {
	mu                     sync.Mutex
	method, path, auth, ct string
	body                   string
}

func (s *seen) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.method, s.path, s.auth, s.ct, s.body = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)
}

// Unit tier: canned httptest responses; no network beyond loopback, no sleeps.
func TestShowSendsAGetForTheEscapedIDAndDecodesTheIssue(t *testing.T) {
	t.Parallel()
	var s seen
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		_, _ = w.Write([]byte(`{"id":"gt-a/b","title":"t"}`))
	}), nil)
	is, err := c.Show(t.Context(), "gt-a/b")
	if err != nil || is.Title != "t" {
		t.Fatalf("Show = %+v, %v", is, err)
	}
	if s.method != "GET" || s.path != "/v1/issues/gt-a%2Fb" || !strings.HasPrefix(s.auth, "Bearer ") || s.body != "" {
		t.Errorf("request = %s %s auth=%q body=%q, want GET with the escaped id, a bearer token and no body", s.method, s.path, s.auth, s.body)
	}
}

func TestShowOfAMissingIDIsNotFound(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, 404, api.NotFound("gt-x"))
	}), nil)
	if _, err := c.Show(t.Context(), "gt-x"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Show missing = %v, want errors.Is api.ErrNotFound", err)
	}
}

func TestShowManySendsTheIDsAndNeverReturnsANilMap(t *testing.T) {
	t.Parallel()
	var s seen
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		_, _ = w.Write([]byte(`{"issues":null}`))
	}), nil)
	m, err := c.ShowMany(t.Context(), []string{"gt-a", "gt-b"})
	if err != nil || m == nil || len(m) != 0 {
		t.Errorf("ShowMany = %v, %v; want an empty non-nil map even when the server sent null", m, err)
	}
	if s.method != "POST" || s.path != "/v1/issues:show-many" || s.ct != "application/json" || s.body != `{"ids":["gt-a","gt-b"]}` {
		t.Errorf("request = %s %s ct=%q body=%q", s.method, s.path, s.ct, s.body)
	}
}

func TestListSendsTheFilterAndMapsInvalid(t *testing.T) {
	t.Parallel()
	var s seen
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		writeErr(w, 400, api.Invalid("bad cursor"))
	}), nil)
	_, err := c.List(t.Context(), api.ListFilter{Assignee: "me", Limit: 3, Cursor: "c"})
	if !errors.Is(err, api.ErrInvalid) {
		t.Errorf("List = %v, want errors.Is api.ErrInvalid", err)
	}
	var f api.ListFilter
	if json.Unmarshal([]byte(s.body), &f) != nil || f.Assignee != "me" || f.Limit != 3 || f.Cursor != "c" || s.path != "/v1/issues:list" {
		t.Errorf("sent %s %q, want the filter in the body", s.path, s.body)
	}
}

func TestCountSendsTheFilterToTheCountRouteAndReadsTheCount(t *testing.T) {
	t.Parallel()
	var s seen
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		_, _ = w.Write([]byte(`{"count":4}`))
	}), nil)
	n, err := c.Count(t.Context(), api.ListFilter{Ephemeral: true, Statuses: []string{"closed"}})
	if err != nil || n != 4 {
		t.Fatalf("Count = %d, %v; want the server's 4", n, err)
	}
	if s.method != "POST" || s.path != "/v1/issues:count" || s.ct != "application/json" ||
		s.body != `{"filter":{"statuses":["closed"],"ephemeral":true}}` {
		t.Errorf("request = %s %s ct=%q body=%q, want a POST of the filter to /v1/issues:count", s.method, s.path, s.ct, s.body)
	}
}

func TestCountOfAFilterThatCarriesAPageIsInvalid(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, 400, api.Invalid("count takes no limit; it counts every issue the filter matches"))
	}), nil)
	if _, err := c.Count(t.Context(), api.ListFilter{Ephemeral: true, Limit: 1}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("Count with a limit = %v, want errors.Is api.ErrInvalid", err)
	}
}

func TestListAllFollowsThreePagesAndStopsAtTheEnd(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var cursors []string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var f api.ListFilter
		_ = json.NewDecoder(r.Body).Decode(&f)
		mu.Lock()
		cursors = append(cursors, f.Cursor)
		mu.Unlock()
		n, _ := strconv.Atoi(strings.TrimPrefix(f.Cursor, "p"))
		page := api.IssuePage{Issues: []api.Issue{{ID: "gt-" + strconv.Itoa(n)}}}
		if n < 2 {
			page.NextCursor = "p" + strconv.Itoa(n+1)
		}
		_ = json.NewEncoder(w).Encode(page)
	}), nil)
	got, err := c.ListAll(t.Context(), api.ListFilter{Limit: 1})
	if err != nil || len(got) != 3 || got[0].ID != "gt-0" || got[2].ID != "gt-2" {
		t.Fatalf("ListAll = %v, %v; want gt-0, gt-1, gt-2", got, err)
	}
	if strings.Join(cursors, ",") != ",p1,p2" {
		t.Errorf("cursors asked = %v, want the empty cursor, then p1, then p2, then stop", cursors)
	}
}
