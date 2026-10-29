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

// TestReadyzNotReadyIsAnAnsweredUnavailableOnce drives the refusal branch: a
// service that answers 503 with its reason is a *NotReady carrying that reason
// and still an unavailable error, and readiness is asked exactly once rather
// than retried.
func TestReadyzNotReadyIsAnAnsweredUnavailableOnce(t *testing.T) {
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
	var nr *NotReady
	if !errors.As(err, &nr) || nr.Reason != "database does not accept writes" {
		t.Fatalf("Readyz error = %v, want the service's own not-ready answer", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want 1: readiness is a point in time, never retried", got)
	}
	if len(sleep.waits) != 0 {
		t.Errorf("waits = %v, want none", sleep.waits)
	}
}

// TestReadyzUnreachable is the transport failure branch: a service that cannot
// be reached is an unavailable error and carries no answer, so it is not a
// *NotReady. The transport fails in process, so the test opens no socket.
func TestReadyzUnreachable(t *testing.T) {
	t.Parallel()
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", Token: "trk_secret-token-value",
		HTTP: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Readyz(t.Context())
	if !errors.Is(err, api.ErrUnavailable) {
		t.Fatalf("Readyz to an unreachable server = %v, want an unavailable error", err)
	}
	var nr *NotReady
	if errors.As(err, &nr) {
		t.Fatalf("Readyz to an unreachable server = %v, want no answer, not a not-ready answer", err)
	}
}

// TestReadyzNotReadyKeepsOtherAnswersApart: only the service's 503 is "not
// ready". A wrong path or a broken proxy answer is the refusal it is, so a
// caller is not told the tracker answered that it is unwell.
func TestReadyzNotReadyKeepsOtherAnswersApart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{name: "a 404 is a wrong endpoint", status: http.StatusNotFound, body: "<html>404 page not found</html>", wantErr: api.ErrInvalid},
		{name: "a 500 without the API's body is unavailable", status: http.StatusInternalServerError, body: "boom", wantErr: api.ErrUnavailable},
		{name: "a 403 is forbidden", status: http.StatusForbidden, body: `{"kind":"forbidden","message":"no"}`, wantErr: api.ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}), nil)
			_, err := c.Readyz(t.Context())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Readyz error = %v, want %v", err, tt.wantErr)
			}
			var nr *NotReady
			if errors.As(err, &nr) {
				t.Errorf("Readyz error = %v, want the refusal itself, not a not-ready answer", err)
			}
		})
	}
}
