package client

import (
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// Unit tier: canned httptest responses; loopback only, no sleeps.
func TestDispatchBoardPostsTheRequestAndFillsNilCollections(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var method, path, body string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		method, path, body = r.Method, r.URL.Path, string(b)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"rigs":{"gt":{"ready":null,"held":null,"children":null}},"probes":null,"errors":null}`))
	}), nil)
	got, err := c.DispatchBoard(t.Context(), api.DispatchBoardRequest{Rigs: []string{"gt"}, ProbeIDs: []string{"gt-r"}})
	if err != nil {
		t.Fatal(err)
	}
	if method != "POST" || path != "/v1/board/dispatch" || body != `{"rigs":["gt"],"probe_ids":["gt-r"]}` {
		t.Errorf("sent %s %s %q", method, path, body)
	}
	gt := got.Rigs["gt"]
	if gt.Ready == nil || gt.Held == nil || gt.Children == nil || got.Probes == nil || got.Errors == nil {
		t.Errorf("DispatchBoard = %+v, want no nil slice or map even when the server sent null", got)
	}
}
