package client

import (
	"errors"
	"net/http"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// TestMetricsReturnsThePageAsServed shows the /metrics text come back
// unparsed, from one GET of the route, through the production client.
func TestMetricsReturnsThePageAsServed(t *testing.T) {
	t.Parallel()
	const page = "# TYPE tracker_requests_total counter\ntracker_requests_total{route=\"GET /readyz\",status=\"200\"} 3\n"
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/metrics" {
			t.Errorf("request = %s %s, want GET /metrics", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}), nil)

	got, err := c.Metrics(t.Context())
	if err != nil {
		t.Fatalf("Metrics = %v", err)
	}
	if got != page {
		t.Errorf("Metrics = %q, want the page as served %q", got, page)
	}
}

// TestMetricsWithoutTheRouteIsAnError drives the refusal branch: a server
// that does not serve /metrics answers 404 with plain text, which is an error
// and never an empty page that would read as zero requests.
func TestMetricsWithoutTheRouteIsAnError(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}), nil)

	got, err := c.Metrics(t.Context())
	if err == nil {
		t.Fatalf("Metrics without the route = %q, nil; want an error", got)
	}
	if got != "" {
		t.Errorf("Metrics without the route returned text %q alongside the error", got)
	}
	if !errors.Is(err, api.ErrInvalid) {
		t.Errorf("Metrics error = %v, want an invalid error for a 404", err)
	}
}
