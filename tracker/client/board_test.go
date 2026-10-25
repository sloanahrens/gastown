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

// Unit tier: canned httptest responses; loopback only, no sleeps.
func TestBoardMethodsPostToTheirPathsWithTheFilter(t *testing.T) {
	t.Parallel()
	for path, call := range map[string]func(c *Client) error{
		"/v1/ready": func(c *Client) error {
			_, err := c.Ready(t.Context(), api.ReadyFilter{Rig: "gt", Limit: 4})
			return err
		},
		"/v1/blocked": func(c *Client) error {
			_, err := c.Blocked(t.Context(), api.ReadyFilter{Rig: "gt", Limit: 4})
			return err
		},
	} {
		var mu sync.Mutex
		var method, gotPath, body, auth string
		c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			method, gotPath, body, auth = r.Method, r.URL.Path, string(b), r.Header.Get("Authorization")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"issues":[]}`))
		}), nil)
		if err := call(c); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var f api.ReadyFilter
		if method != "POST" || gotPath != path || json.Unmarshal([]byte(body), &f) != nil || f.Rig != "gt" || f.Limit != 4 || !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("%s: sent %s %s %q auth=%q, want POST with the filter and a bearer token", path, method, gotPath, body, auth)
		}
	}
}

func TestBoardErrorsMapToAPIKinds(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, 400, api.Invalid("bad cursor"))
	}), nil)
	if _, err := c.Ready(t.Context(), api.ReadyFilter{}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("Ready = %v, want api.ErrInvalid", err)
	}
	if _, err := c.Blocked(t.Context(), api.ReadyFilter{}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("Blocked = %v, want api.ErrInvalid", err)
	}
	if _, err := c.ChildrenOf(t.Context(), []string{"gt-p"}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("ChildrenOf = %v, want api.ErrInvalid", err)
	}
}

func TestChildrenOfSendsParentsAndNeverReturnsANilMap(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var body string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"children":null}`))
	}), nil)
	m, err := c.ChildrenOf(t.Context(), []string{"gt-a", "gt-b"})
	if err != nil || m == nil || len(m) != 0 {
		t.Errorf("ChildrenOf = %v, %v; want an empty non-nil map even when the server sent null", m, err)
	}
	if body != `{"parents":["gt-a","gt-b"]}` {
		t.Errorf("body = %q, want the parents", body)
	}
}

func TestReadyAllFollowsThreePagesAndStops(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var cursors []string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var f api.ReadyFilter
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
	got, err := c.ReadyAll(t.Context(), api.ReadyFilter{Limit: 1})
	if err != nil || len(got) != 3 || got[2].ID != "gt-2" {
		t.Fatalf("ReadyAll = %v, %v; want gt-0..gt-2", got, err)
	}
	if strings.Join(cursors, ",") != ",p1,p2" {
		t.Errorf("cursors asked = %v, want empty, p1, p2 then stop", cursors)
	}
}

func TestReadyAllReturnsAnErrorNotAPartialListWhenAMiddlePageFails(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var f api.ReadyFilter
		_ = json.NewDecoder(r.Body).Decode(&f)
		switch f.Cursor {
		case "":
			_, _ = w.Write([]byte(`{"issues":[{"id":"gt-0"}],"next_cursor":"p1"}`))
		default:
			writeErr(w, 400, api.Invalid("bad cursor"))
		}
	}), nil)
	got, err := c.ReadyAll(t.Context(), api.ReadyFilter{})
	if !errors.Is(err, api.ErrInvalid) || got != nil {
		t.Errorf("ReadyAll = %v, %v; want nil and api.ErrInvalid, not the first page alone", got, err)
	}
}

func TestUnavailableKeepsItsKindThroughTheBoardMethods(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, 503, api.Unavailable("database does not answer"))
	}), func(cfg *Config) { cfg.MaxRetries = 1 })
	if _, err := c.Ready(t.Context(), api.ReadyFilter{}); !errors.Is(err, api.ErrUnavailable) {
		t.Errorf("Ready = %v, want api.ErrUnavailable", err)
	}
	if _, err := c.ChildrenOf(t.Context(), []string{"gt-a"}); !errors.Is(err, api.ErrUnavailable) {
		t.Errorf("ChildrenOf = %v, want api.ErrUnavailable", err)
	}
}

func TestChildrenOfWithNoParentsIsAnEmptyNonNilMap(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"children":{}}`))
	}), nil)
	m, err := c.ChildrenOf(t.Context(), nil)
	if err != nil || m == nil || len(m) != 0 {
		t.Errorf("ChildrenOf(nil) = %v, %v; want an empty non-nil map", m, err)
	}
}
