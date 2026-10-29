package client

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// TestReadyzReadsTheAnswer shows a ready service answering 200 with its
// schema level, through the production client and a real request path.
func TestReadyzReadsTheAnswer(t *testing.T) {
	t.Parallel()
	var paths atomic.Int32
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths.Add(1)
		if r.URL.Path != "/readyz" {
			t.Errorf("path = %q, want /readyz", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready","schema_level":3}`))
	}), nil)

	got, err := c.Readyz(t.Context())
	if err != nil {
		t.Fatalf("Readyz = %v", err)
	}
	if got.Status != "ready" || got.SchemaLevel != 3 {
		t.Errorf("Readyz = %+v, want ready at schema level 3", got)
	}
}

// TestReadyzNotReadyIsUnavailableOnce drives the refusal branch: a service
// that answers 503 with its reason is an unavailable error carrying that
// reason, and readiness is asked exactly once rather than retried.
func TestReadyzNotReadyIsUnavailableOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, sleep := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"kind":"unavailable","message":"database does not accept writes"}`))
	}), nil)

	_, err := c.Readyz(t.Context())
	if !errors.Is(err, api.ErrUnavailable) {
		t.Fatalf("Readyz error = %v, want an unavailable error", err)
	}
	var e *api.Error
	if !errors.As(err, &e) || e.Message != "database does not accept writes" {
		t.Errorf("Readyz error = %v, want the server's reason", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want 1: readiness is a point in time, never retried", got)
	}
	if len(sleep.waits) != 0 {
		t.Errorf("waits = %v, want none", sleep.waits)
	}
}

// TestReadyzUnreachable is the transport failure branch: a service that cannot
// be reached is an unavailable error, not a nil answer. The transport fails in
// process, so the test opens no socket.
func TestReadyzUnreachable(t *testing.T) {
	t.Parallel()
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", Token: "trk_secret-token-value",
		HTTP: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Readyz(t.Context()); !errors.Is(err, api.ErrUnavailable) {
		t.Fatalf("Readyz to an unreachable server = %v, want an unavailable error", err)
	}
}
