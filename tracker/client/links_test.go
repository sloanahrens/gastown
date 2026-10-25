package client

import (
	"net/http"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// Unit tier: canned httptest responses; loopback only, no sleeps.
func TestLinkMethodsEscapePathsAndSendTheRightRequests(t *testing.T) {
	t.Parallel()
	type seenReq struct{ method, path, query string }
	var mu sync.Mutex
	var seen []seenReq
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, seenReq{r.Method, r.URL.EscapedPath(), r.URL.RawQuery})
		mu.Unlock()
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"comments":null,"dependencies":null}`))
			return
		}
		w.WriteHeader(204)
	}), nil)
	ctx := t.Context()
	if err := c.AddComment(ctx, "gt-a b", "me", "x"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Comments(ctx, "gt-a"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("Comments = %v, %v; want an empty non-nil slice even when the server sent null", got, err)
	}
	if err := c.AddDependency(ctx, "gt-a", "gt-b", "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDependency(ctx, "gt-a", "external:om:om-7"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDependency(ctx, "gt-a", "has space"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Dependencies(ctx, "gt-a", ""); err != nil || got == nil || len(got) != 0 {
		t.Errorf("Dependencies = %v, %v; want an empty non-nil slice", got, err)
	}
	if _, err := c.Dependencies(ctx, "gt-a", "tracks"); err != nil {
		t.Fatal(err)
	}
	want := []seenReq{
		{"POST", "/v1/issues/gt-a%20b/comments", ""},
		{"GET", "/v1/issues/gt-a/comments", ""},
		{"POST", "/v1/issues/gt-a/dependencies", ""},
		{"DELETE", "/v1/issues/gt-a/dependencies/external:om:om-7", ""},
		{"DELETE", "/v1/issues/gt-a/dependencies/has%20space", ""},
		{"GET", "/v1/issues/gt-a/dependencies", ""},
		{"GET", "/v1/issues/gt-a/dependencies", "type=tracks"},
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(want) {
		t.Fatalf("saw %d requests, want %d: %+v", len(seen), len(want), seen)
	}
	for i, w := range want {
		if seen[i] != w {
			t.Errorf("request %d = %+v, want %+v", i, seen[i], w)
		}
	}
}

func TestLinkWritesAreNeverRetriedButLinkReadsAre(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	calls := map[string]int{}
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.Method]++
		mu.Unlock()
		writeErr(w, 503, api.Unavailable("database does not answer"))
	}), func(cfg *Config) { cfg.MaxRetries = 2 })
	ctx := t.Context()
	_ = c.AddComment(ctx, "gt-a", "", "x")
	_ = c.AddDependency(ctx, "gt-a", "gt-b", "blocks")
	_ = c.RemoveDependency(ctx, "gt-a", "gt-b")
	_, _ = c.Comments(ctx, "gt-a")
	mu.Lock()
	defer mu.Unlock()
	if calls["POST"] != 2 || calls["DELETE"] != 1 {
		t.Errorf("write attempts = %v, want one each (2 POST, 1 DELETE)", calls)
	}
	if calls["GET"] != 3 {
		t.Errorf("read attempts = %d, want 3 (1 + 2 retries)", calls["GET"])
	}
}
