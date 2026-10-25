package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

type fakeAuth map[string]string // token -> actor

func (f fakeAuth) ActorForToken(_ context.Context, token string) (string, error) {
	if a, ok := f[token]; ok {
		return a, nil
	}
	return "", api.ErrUnknownToken
}

type fakeHealth struct{ ping, write error }

func (f fakeHealth) Ping(context.Context) error       { return f.ping }
func (f fakeHealth) WriteProbe(context.Context) error { return f.write }

func newTestServer(t *testing.T, h Health) *httptest.Server {
	t.Helper()
	s := New(Config{Auth: fakeAuth{"good-token": "town/crew/dipper"}, Health: h, SchemaLevel: 3})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func decodeErr(t *testing.T, body string) api.Error {
	t.Helper()
	var e api.Error
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("body %q is not an api.Error: %v", body, err)
	}
	return e
}

// Unit tier: httptest on loopback, in-process fakes, milliseconds per case.
func TestProbesNeedNoToken(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, fakeHealth{})
	for path, want := range map[string]string{"/healthz": "ok", "/readyz": `"status":"ready"`, "/version": `"schema_level":3`} {
		code, body := get(t, ts.URL+path, nil)
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("GET %s = %d %q, want 200 containing %q", path, code, body, want)
		}
	}
}

func TestReadyzReportsEachFailure(t *testing.T) {
	t.Parallel()
	for name, h := range map[string]fakeHealth{
		"database down":    {ping: errors.New("connection refused")},
		"read-only server": {write: errors.New("cannot assign TransactionIds during recovery")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, body := get(t, newTestServer(t, h).URL+"/readyz", nil)
			if code != 503 {
				t.Fatalf("status = %d, want 503", code)
			}
			if e := decodeErr(t, body); e.Kind != api.KindUnavailable || e.Message == "" {
				t.Errorf("body = %+v, want an unavailable error with a message", e)
			}
		})
	}
}

func TestV1RequiresAValidBearerToken(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, fakeHealth{})
	schema := map[string]string{api.SchemaHeader: "3"}
	with := func(auth string) map[string]string {
		h := map[string]string{api.SchemaHeader: "3"}
		if auth != "" {
			h["Authorization"] = auth
		}
		return h
	}
	for name, tc := range map[string]struct {
		hdr  map[string]string
		code int
	}{
		"no token":        {schema, 401},
		"wrong scheme":    {with("Basic good-token"), 401},
		"unknown token":   {with("Bearer nope"), 401},
		"empty bearer":    {with("Bearer "), 401},
		"good token":      {with("Bearer good-token"), 200},
		"lower-case type": {with("bearer good-token"), 200},
	} {
		code, body := get(t, ts.URL+"/v1/whoami", tc.hdr)
		if code != tc.code {
			t.Errorf("%s: status = %d (%s), want %d", name, code, body, tc.code)
		}
	}
	code, body := get(t, ts.URL+"/v1/whoami", with("Bearer good-token"))
	if code != 200 || !strings.Contains(body, `"actor":"town/crew/dipper"`) {
		t.Errorf("whoami = %d %q, want the actor from the token", code, body)
	}
}

func TestUnauthorizedBodyIsATypedError(t *testing.T) {
	t.Parallel()
	_, body := get(t, newTestServer(t, fakeHealth{}).URL+"/v1/whoami", map[string]string{api.SchemaHeader: "3"})
	if e := decodeErr(t, body); e.Kind != api.KindInvalid || !strings.Contains(e.Message, "token") {
		t.Errorf("401 body = %+v, want a typed error that mentions the token", e)
	}
}

func TestSchemaSkewIsRefusedBeforeAnyRoute(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, fakeHealth{})
	auth := "Bearer good-token"
	for name, tc := range map[string]struct {
		schema string
		code   int
		want   string
	}{
		"missing":      {"", 400, "required"},
		"too old":      {"2", 409, "skew"},
		"too new":      {"4", 409, "skew"},
		"not a number": {"x", 400, "integer"},
		"matches":      {"3", 200, ""},
	} {
		hdr := map[string]string{"Authorization": auth}
		if tc.schema != "" {
			hdr[api.SchemaHeader] = tc.schema
		}
		code, body := get(t, ts.URL+"/v1/whoami", hdr)
		if code != tc.code || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %q, want %d containing %q", name, code, body, tc.code, tc.want)
		}
	}
}

func TestInternalErrorsDoNotLeakTheirCause(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writeError(rec, errors.New("pq: password authentication failed for user tracker"), discardLog{})
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Errorf("body %q leaks the internal cause", rec.Body.String())
	}
	if e := decodeErr(t, rec.Body.String()); e.Kind != api.KindUnavailable {
		t.Errorf("kind = %q, want unavailable", e.Kind)
	}
}

func TestAPIErrorsKeepTheirKindAndStatus(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writeError(rec, api.NotFound("gt-9"), discardLog{})
	if rec.Code != 404 || decodeErr(t, rec.Body.String()).Kind != api.KindNotFound {
		t.Errorf("got %d %s, want a 404 not_found", rec.Code, rec.Body.String())
	}
}

func TestRequireLoopback(t *testing.T) {
	t.Parallel()
	for addr, ok := range map[string]bool{
		"127.0.0.1:7433": true, "[::1]:7433": true, "127.0.0.1:0": true,
		"0.0.0.0:7433": false, ":7433": false, "192.168.1.5:7433": false, "localhost:7433": false, "example.com:80": false, "7433": false,
	} {
		err := RequireLoopback(addr)
		if (err == nil) != ok {
			t.Errorf("RequireLoopback(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

type recLog struct {
	mu   sync.Mutex
	logs []string
}

func (r *recLog) Printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *recLog) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n")
}

// /readyz is unauthenticated: its body must not carry the database's own
// error text (a host name, a user name), which belongs in the log.
func TestReadyzBodyNeverCarriesTheDatabaseError(t *testing.T) {
	t.Parallel()
	for name, h := range map[string]fakeHealth{
		"ping":  {ping: errors.New("dial tcp 10.1.2.3:5432: password authentication failed for user tracker_admin")},
		"write": {write: errors.New("cannot execute INSERT in a read-only transaction (host db-internal)")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := &recLog{}
			s := New(Config{Auth: fakeAuth{}, Health: h, SchemaLevel: 1, Log: log})
			ts := httptest.NewServer(s.Handler())
			t.Cleanup(ts.Close)
			code, body := get(t, ts.URL+"/readyz", nil)
			if code != 503 {
				t.Fatalf("status = %d, want 503", code)
			}
			for _, leak := range []string{"10.1.2.3", "tracker_admin", "db-internal", "read-only", "password"} {
				if strings.Contains(body, leak) {
					t.Errorf("body %q leaks %q", body, leak)
				}
			}
			if !strings.Contains(log.all(), "readyz") || (!strings.Contains(log.all(), "tracker_admin") && !strings.Contains(log.all(), "db-internal")) {
				t.Errorf("the cause must be in the log, got %q", log.all())
			}
		})
	}
}

// Skew is reported only to a caller who authenticated.
func TestSchemaSkewIsNotDisclosedWithoutAToken(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, fakeHealth{})
	code, body := get(t, ts.URL+"/v1/whoami", map[string]string{api.SchemaHeader: "99"})
	if code != 401 || strings.Contains(body, "skew") {
		t.Errorf("unauthenticated skewed request = %d %q, want a plain 401 that says nothing of the schema level", code, body)
	}
}

// The real client against the real server handler: the headers, the schema
// level, the token and the error body all line up. A compatibility check, not
// a second test of either side.
func TestClientAndServerAgree(t *testing.T) {
	t.Parallel()
	s := New(Config{Auth: fakeAuth{"trk_good": "town/crew/dipper"}, Health: fakeHealth{}, SchemaLevel: api.SchemaLevel})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	good, err := client.New(client.Config{BaseURL: ts.URL, Token: "trk_good"})
	if err != nil {
		t.Fatal(err)
	}
	if actor, err := good.Whoami(context.Background()); err != nil || actor != "town/crew/dipper" {
		t.Errorf("Whoami = %q, %v", actor, err)
	}

	bad, _ := client.New(client.Config{BaseURL: ts.URL, Token: "trk_bad", MaxRetries: -1})
	if _, err := bad.Whoami(context.Background()); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("a wrong token = %v, want the server's typed invalid error", err)
	}

	skewed, _ := client.New(client.Config{BaseURL: ts.URL, Token: "trk_good", SchemaLevel: api.SchemaLevel + 1, MaxRetries: -1})
	if _, err := skewed.Whoami(context.Background()); !errors.Is(err, api.ErrConflict) {
		t.Errorf("a skewed client = %v, want the server's conflict error", err)
	}
}
