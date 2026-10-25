package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
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
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("saw %d requests, want 4: %+v", len(seen), seen)
	}
	// method, path and the decoded body of each request, compared field by field.
	type want struct {
		method, path string
		body         map[string]any
	}
	for i, w := range []want{
		{"POST", "/v1/issues", map[string]any{"title": "t", "rig": "gt"}},
		{"PATCH", "/v1/issues/gt-a%2Fb%20c", map[string]any{"patch": map[string]any{"title": "n"}, "guard": map[string]any{}}},
		{"POST", "/v1/issues:close", map[string]any{"ids": []any{"gt-a", "gt-b"}, "reason": "done", "force": true}},
		{"POST", "/v1/issues:delete", map[string]any{"ids": []any{"gt-a"}}},
	} {
		if seen[i].method != w.method || seen[i].path != w.path {
			t.Errorf("request %d = %s %s, want %s %s", i, seen[i].method, seen[i].path, w.method, w.path)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(seen[i].body), &got); err != nil {
			t.Fatalf("request %d body %q is not JSON: %v", i, seen[i].body, err)
		}
		// Every key the test names must match; keys the wire adds with their
		// zero value (priority 0, an empty patch field) are not asserted.
		if !containsAll(got, w.body) {
			t.Errorf("request %d body = %v, want it to contain %v", i, got, w.body)
		}
	}
}

// containsAll reports whether got has every key of want with an equal value,
// recursing into nested objects.
func containsAll(got, want map[string]any) bool {
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			return false
		}
		if wm, isMap := wv.(map[string]any); isMap {
			gm, ok := gv.(map[string]any)
			if !ok || !containsAll(gm, wm) {
				return false
			}
			continue
		}
		if !reflect.DeepEqual(gv, wv) {
			return false
		}
	}
	return true
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
