package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// Unit tier: canned httptest responses; loopback only, no sleeps (the fake
// sleep records waits and returns at once).
func TestWriteMethodsSendTheRightRequestsAndHandle201And204(t *testing.T) {
	t.Parallel()
	type seenReq struct{ method, path, body string }
	var mu sync.Mutex
	var seen []seenReq
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenReq{r.Method, r.URL.EscapedPath(), string(b)})
		mu.Unlock()
		if r.Method == "POST" && r.URL.Path == "/v1/issues" {
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"gt-new","title":"t"}`))
			return
		}
		w.WriteHeader(204)
	}), nil)
	ctx := t.Context()
	is, err := c.Create(ctx, api.CreateParams{Title: "t", Rig: "gt"})
	if err != nil || is.ID != "gt-new" {
		t.Fatalf("Create = %+v, %v; want the issue from the 201 body", is, err)
	}
	title := "n"
	for name, err := range map[string]error{
		"Update": c.Update(ctx, "gt-a/b c", api.UpdatePatch{Title: &title}, api.Guard{}),
		"Close":  c.Close(ctx, []string{"gt-a", "gt-b"}, "done", true),
		"Delete": c.Delete(ctx, []string{"gt-a"}),
	} {
		if err != nil {
			t.Errorf("%s on 204 = %v, want nil", name, err)
		}
	}
	want := []seenReq{
		{"POST", "/v1/issues", `{"title":"t","rig":"gt","priority":0}`},
		{"PATCH", "/v1/issues/gt-a%2Fb%20c", `{"patch":{"title":"n","priority":null},"guard":{}}`},
		{"POST", "/v1/issues:close", `{"ids":["gt-a","gt-b"],"reason":"done","force":true}`},
		{"POST", "/v1/issues:delete", `{"ids":["gt-a"]}`},
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(want) {
		t.Fatalf("saw %d requests, want %d: %+v", len(seen), len(want), seen)
	}
	for i, w := range want {
		if seen[i].method != w.method || seen[i].path != w.path {
			t.Errorf("request %d = %s %s, want %s %s", i, seen[i].method, seen[i].path, w.method, w.path)
		}
		var a, b any
		if json.Unmarshal([]byte(seen[i].body), &a) != nil || json.Unmarshal([]byte(w.body), &b) != nil {
			t.Fatalf("request %d body %q or want %q is not JSON", i, seen[i].body, w.body)
		}
	}
}

func TestAWriteThatGets503IsAttemptedExactlyOnce(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	c, fs := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writeErr(w, 503, api.Unavailable("database does not answer"))
	}), func(cfg *Config) { cfg.MaxRetries = 5 })
	ctx := t.Context()
	title := "x"
	for name, err := range map[string]error{
		"Create": func() error { _, err := c.Create(ctx, api.CreateParams{Title: "t"}); return err }(),
		"Update": c.Update(ctx, "gt-a", api.UpdatePatch{Title: &title}, api.Guard{}),
		"Close":  c.Close(ctx, []string{"gt-a"}, "", false),
		"Delete": c.Delete(ctx, []string{"gt-a"}),
	} {
		if !errors.Is(err, api.ErrUnavailable) {
			t.Errorf("%s = %v, want api.ErrUnavailable", name, err)
		}
	}
	if n := attempts.Load(); n != 4 {
		t.Errorf("server saw %d attempts for 4 writes, want exactly 4 (a write is never retried)", n)
	}
	if len(fs.waits) != 0 {
		t.Errorf("client waited %v between attempts; a write has no retries to wait for", fs.waits)
	}
}
