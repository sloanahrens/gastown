package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/internal/leakcheck"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Main(m)) }

// fakeSleep records the waits and returns at once: no test sleeps.
type fakeSleep struct{ waits []time.Duration }

func (f *fakeSleep) sleep(_ context.Context, d time.Duration) error {
	f.waits = append(f.waits, d)
	return nil
}

// newClient builds the client under test against a server of its own, on a
// transport of its own with keep-alives off: httptest's Server.Close closes
// http.DefaultTransport's idle connections, so on the shared transport a
// parallel test's cleanup takes the connection a write is using, and a write
// gets no retry (gt-ez3ju).
func newClient(t *testing.T, h http.Handler, mod func(*Config)) (*Client, *fakeSleep) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	fs := &fakeSleep{}
	cfg := Config{BaseURL: ts.URL, Token: "trk_secret-token-value", Sleep: fs.sleep,
		HTTP: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, fs
}

// The gate's order: a write, a sibling test's cleanup (its server's Close), a
// write. Both writes must land (gt-ez3ju).
func TestAWriteLandsAfterASiblingServersClose(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	ctx := t.Context()
	if err := c.Close(ctx, []string{"gt-a"}, "done", false); err != nil {
		t.Fatalf("first write = %v", err)
	}
	sibling := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	sibling.Close() // what the sibling test's t.Cleanup(ts.Close) does
	if err := c.Close(ctx, []string{"gt-b"}, "done", false); err != nil {
		t.Fatalf("write after a sibling server closed = %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, e *api.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"kind":"` + string(e.Kind) + `","message":"` + e.Message + `"}`))
}

// Unit tier: httptest on loopback and an injected sleep; milliseconds per case.
func TestRequestsCarryTheTokenAndSchemaLevel(t *testing.T) {
	t.Parallel()
	var gotAuth, gotSchema, gotTag string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotSchema, gotTag = r.Header.Get("Authorization"), r.Header.Get(api.SchemaHeader), r.Header.Get(api.RequestTagHeader)
		_, _ = w.Write([]byte(`{"actor":"town/crew/dipper"}`))
	}), nil)
	actor, err := c.Whoami(WithTag(context.Background(), "tick-7"))
	if err != nil || actor != "town/crew/dipper" {
		t.Fatalf("Whoami = %q, %v", actor, err)
	}
	if gotAuth != "Bearer trk_secret-token-value" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
	if gotSchema != strconv.Itoa(api.SchemaLevel) {
		t.Errorf("%s = %q, want the api schema level", api.SchemaHeader, gotSchema)
	}
	if gotTag != "tick-7" {
		t.Errorf("%s = %q, want the context tag", api.RequestTagHeader, gotTag)
	}
}

func TestAPIErrorsComeBackTyped(t *testing.T) {
	t.Parallel()
	for status, kind := range map[int]api.Kind{404: api.KindNotFound, 409: api.KindGuardFailed, 400: api.KindInvalid} {
		c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, status, &api.Error{Kind: kind, Message: "m"})
		}), nil)
		_, err := c.Whoami(context.Background())
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Kind != kind || ae.Message != "m" {
			t.Errorf("status %d: error = %v, want a typed %s error", status, err, kind)
		}
	}
}

func TestRetriesOnlyUnavailableAndOnlyUpToTheCap(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, fs := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeErr(w, 503, api.Unavailable("busy"))
	}), func(cfg *Config) { cfg.MaxRetries = 2 })
	_, err := c.Whoami(context.Background())
	if !errors.Is(err, api.ErrUnavailable) {
		t.Fatalf("error = %v, want unavailable after the retries are spent", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (one try plus MaxRetries 2)", calls.Load())
	}
	if len(fs.waits) != 2 || fs.waits[1] <= fs.waits[0] {
		t.Errorf("waits = %v, want two increasing backoffs", fs.waits)
	}

	for _, kind := range []api.Kind{api.KindNotFound, api.KindInvalid, api.KindGuardFailed, api.KindConflict} {
		var n atomic.Int32
		c, fs := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n.Add(1)
			writeErr(w, (&api.Error{Kind: kind}).HTTPStatus(), &api.Error{Kind: kind, Message: "no"})
		}), nil)
		_, _ = c.Whoami(context.Background())
		if n.Load() != 1 || len(fs.waits) != 0 {
			t.Errorf("%s was retried (%d calls, waits %v); only unavailable is", kind, n.Load(), fs.waits)
		}
	}
}

func TestARetryThatSucceedsReturnsTheResult(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writeErr(w, 503, api.Unavailable("warming up"))
			return
		}
		_, _ = w.Write([]byte(`{"actor":"a"}`))
	}), nil)
	if actor, err := c.Whoami(context.Background()); err != nil || actor != "a" {
		t.Errorf("Whoami = %q, %v; want the second attempt's answer", actor, err)
	}
}

func TestWritesAreNeverRetried(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, fs := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeErr(w, 503, api.Unavailable("busy"))
	}), nil)
	err := c.do(context.Background(), http.MethodPost, "/v1/anything", map[string]string{"a": "b"}, nil, false)
	if !errors.Is(err, api.ErrUnavailable) || calls.Load() != 1 || len(fs.waits) != 0 {
		t.Errorf("a non-idempotent call: err=%v calls=%d waits=%v; want one attempt, no retry (the write may have landed)", err, calls.Load(), fs.waits)
	}
}

// rtFunc is an http.RoundTripper for tests: no sockets, no clock.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func TestEachAttemptCarriesAnAbsoluteDeadlineOfTheConfiguredTimeout(t *testing.T) {
	t.Parallel()
	var deadline time.Time
	var ok bool
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", Token: "trk_t", Timeout: 37 * time.Second, MaxRetries: -1,
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			deadline, ok = r.Context().Deadline()
			return respond(200, `{"actor":"a"}`), nil
		})}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ok || deadline.Before(start.Add(36*time.Second)) || deadline.After(time.Now().Add(38*time.Second)) {
		t.Errorf("attempt deadline = %v (set %v), want about 37 s after the call began: the per-attempt timeout is not applied", deadline, ok)
	}
}

func TestAnAttemptThatTimesOutIsUnavailableButACanceledCallerIsNot(t *testing.T) {
	t.Parallel()
	timesOut := rtFunc(func(r *http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })
	c, _ := New(Config{BaseURL: "http://127.0.0.1:1", Token: "trk_t", MaxRetries: -1, HTTP: &http.Client{Transport: timesOut}})
	if _, err := c.Whoami(context.Background()); !errors.Is(err, api.ErrUnavailable) {
		t.Errorf("an attempt that timed out = %v, want unavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	counted := rtFunc(func(r *http.Request) (*http.Response, error) { calls.Add(1); return nil, r.Context().Err() })
	c, _ = New(Config{BaseURL: "http://127.0.0.1:1", Token: "trk_t", HTTP: &http.Client{Transport: counted}})
	_, err := c.Whoami(ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, api.ErrUnavailable) {
		t.Errorf("a canceled caller = %v, want context.Canceled and not unavailable", err)
	}
	if calls.Load() > 1 {
		t.Errorf("a canceled caller was retried (%d attempts)", calls.Load())
	}
}

// A 2xx whose body cannot be read may mean the call was applied: it must not
// look retryable.
func TestAMalformedSuccessBodyIsNotRetryable(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := New(Config{BaseURL: "http://127.0.0.1:1", Token: "trk_t", Sleep: func(context.Context, time.Duration) error { return nil },
		HTTP: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return respond(200, "<html>proxy page"), nil
		})}})
	_, err := c.Whoami(context.Background())
	if err == nil || errors.Is(err, api.ErrUnavailable) {
		t.Errorf("malformed 2xx = %v, want a non-retryable error", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1: a success must not be repeated", calls.Load())
	}
}

func TestServerErrorTextIsCleanedBeforeItIsReturned(t *testing.T) {
	t.Parallel()
	const tok = "trk_secret-token-value"
	body := `{"kind":"invalid","message":"bad\nline ` + tok + ` ` + strings.Repeat("x", 800) + `"}`
	c, _ := New(Config{BaseURL: "http://127.0.0.1:1", Token: tok, MaxRetries: -1,
		HTTP: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) { return respond(400, body), nil })}})
	_, err := c.Whoami(context.Background())
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Kind != api.KindInvalid {
		t.Fatalf("error = %v, want the typed invalid error", err)
	}
	if strings.Contains(ae.Message, tok) || strings.ContainsAny(ae.Message, "\n\r") || len(ae.Message) > 510 {
		t.Errorf("message %q must have no token, no control characters and a bounded length", ae.Message)
	}
}

// A secret must not leak through any error path.
func TestTheTokenNeverAppearsInAnError(t *testing.T) {
	t.Parallel()
	const secret = "trk_secret-token-value"
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "echo: "+r.Header.Get("Authorization"), 500) // a hostile or buggy server
	}), func(cfg *Config) { cfg.MaxRetries = -1 })
	_, err := c.Whoami(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("error %v must exist and must not contain the token", err)
	}

	missing := filepath.Join(t.TempDir(), "absent")
	_, err = New(Config{BaseURL: "http://127.0.0.1:1", TokenFile: missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("a missing token file = %v, want an error naming the path", err)
	}
}

func TestTokenSources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	if err := os.WriteFile(f, []byte("trk_from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", TokenFile: f})
	if err != nil || c.token != "trk_from-file" {
		t.Errorf("token from file = %q, %v; want it trimmed", c.token, err)
	}
	if _, err := New(Config{BaseURL: "http://127.0.0.1:1"}); err == nil {
		t.Error("New with no token source succeeded")
	}
	if _, err := New(Config{BaseURL: "http://127.0.0.1:1", Token: "x", TokenFile: f}); err == nil {
		t.Error("New with two token sources succeeded; the source must be unambiguous")
	}
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte("\n"), 0o600)
	if _, err := New(Config{BaseURL: "http://127.0.0.1:1", TokenFile: empty}); err == nil {
		t.Error("an empty token file was accepted")
	}
}

func TestBaseURLMustBeLoopbackUnlessTLS(t *testing.T) {
	t.Parallel()
	for url, ok := range map[string]bool{
		"http://127.0.0.1:7433": true, "http://[::1]:7433": true,
		"https://tracker.internal:443": true,
		"http://tracker.internal:7433": false, "http://10.0.0.5:7433": false, "ftp://x": false, "": false,
	} {
		_, err := New(Config{BaseURL: url, Token: "t"})
		if (err == nil) != ok {
			t.Errorf("New(BaseURL=%q) error = %v, want ok=%v: plain http leaves the host only to loopback", url, err, ok)
		}
	}
}

// Plain http reaches the loopback IPs and the one allowed host, and no other
// host; an allowed value that is a wildcard, a port or a scheme is refused.
func TestPlainHTTPReachesOnlyLoopbackAndTheAllowedHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, base, allowed string
		ok                  bool
	}{
		{"loopback IPv4", "http://127.0.0.1:7433", "", true},
		{"loopback IPv6", "http://[::1]:7433", "", true},
		{"allowed host", "http://tracker.internal:7433", "tracker.internal", true},
		{"allowed host in another case", "http://Tracker.Internal:7433", "tracker.internal", true},
		{"no allowed host", "http://tracker.internal:7433", "", false},
		{"other host", "http://other.internal:7433", "tracker.internal", false},
		{"allowed host as a prefix of another host", "http://tracker.internal.evil:7433", "tracker.internal", false},
		{"allowed host as a suffix of another host", "http://evil.tracker.internal:7433", "tracker.internal", false},
		{"wildcard subdomain value", "http://sub.tracker.internal:7433", "*.tracker.internal", false},
		{"bare wildcard value", "http://tracker.internal:7433", "*", false},
		{"bare wildcard value reaches any host", "http://anything.example:7433", "*", false},
		{"allowed value with a port", "http://tracker.internal:7433", "tracker.internal:7433", false},
		{"allowed value with a scheme", "http://tracker.internal:7433", "http://tracker.internal", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Config{BaseURL: c.base, Token: "t", AllowHTTPHost: c.allowed})
			if (err == nil) != c.ok {
				t.Errorf("New(BaseURL=%q, AllowHTTPHost=%q) error = %v, want ok=%v", c.base, c.allowed, err, c.ok)
			}
		})
	}
}

// https reaches any host whatever the allowed host is, so the allowed host
// adds nothing to a TLS base URL.
func TestHTTPSIsUnchangedByTheAllowedHost(t *testing.T) {
	t.Parallel()
	for _, allowed := range []string{"", "tracker.internal", "other.internal"} {
		for _, base := range []string{"https://tracker.internal:443", "https://elsewhere.example:443"} {
			if _, err := New(Config{BaseURL: base, Token: "t", AllowHTTPHost: allowed}); err != nil {
				t.Errorf("New(BaseURL=%q, AllowHTTPHost=%q) = %v, want https accepted", base, allowed, err)
			}
		}
	}
}

// A wildcard, port, scheme or path in the allowed value is a configuration
// error for any base URL, so a misconfiguration is seen, not silently matched.
func TestAMalformedAllowedHostIsRefusedWhateverTheScheme(t *testing.T) {
	t.Parallel()
	for _, allowed := range []string{"*", "*.tracker.internal", "tracker.internal:7433", "http://tracker.internal", "tracker.internal/x"} {
		for _, base := range []string{"http://tracker.internal:7433", "https://tracker.internal:443"} {
			if _, err := New(Config{BaseURL: base, Token: "t", AllowHTTPHost: allowed}); err == nil {
				t.Errorf("New(BaseURL=%q, AllowHTTPHost=%q) succeeded, want the malformed allowed host refused", base, allowed)
			}
		}
	}
}

func TestFromEnvCarriesTheAllowedHostIntoPlainHTTP(t *testing.T) {
	t.Parallel()
	cfg := FromEnv(func(k string) string {
		return map[string]string{"TRACKER_URL": "http://tracker.internal:7433", "TRACKER_TOKEN": "t", "TRACKER_ALLOW_HTTP_HOST": "tracker.internal"}[k]
	})
	if cfg.AllowHTTPHost != "tracker.internal" {
		t.Fatalf("FromEnv AllowHTTPHost = %q, want the TRACKER_ALLOW_HTTP_HOST value", cfg.AllowHTTPHost)
	}
	if _, err := New(cfg); err != nil {
		t.Errorf("New(FromEnv) = %v, want plain http to the allowed host accepted", err)
	}
}

func TestAllPagesThroughTheCursorAndStopsOnItsAbsence(t *testing.T) {
	t.Parallel()
	pages := map[string]struct {
		items []int
		next  string
	}{"": {[]int{1, 2}, "a"}, "a": {[]int{3}, "b"}, "b": {[]int{4, 5}, ""}}
	var asked []string
	got, err := All(context.Background(), func(_ context.Context, cursor string) ([]int, string, error) {
		asked = append(asked, cursor)
		p := pages[cursor]
		return p.items, p.next, nil
	})
	if err != nil || len(got) != 5 || got[4] != 5 {
		t.Fatalf("All = %v, %v; want 1..5", got, err)
	}
	if strings.Join(asked, ",") != ",a,b" {
		t.Errorf("cursors asked = %q, want the empty cursor then a then b", asked)
	}
}

func TestAllRefusesACursorLoopAndSurfacesAnError(t *testing.T) {
	t.Parallel()
	_, err := All(context.Background(), func(_ context.Context, cursor string) ([]int, string, error) {
		return []int{1}, "same", nil
	})
	if err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Errorf("a server that never ends its pages: %v, want an error that names the cursor", err)
	}
	boom := errors.New("boom")
	_, err = All(context.Background(), func(context.Context, string) ([]int, string, error) { return nil, "", boom })
	if !errors.Is(err, boom) {
		t.Errorf("All swallowed the fetch error: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Parallel()
	cfg := FromEnv(func(k string) string {
		return map[string]string{"TRACKER_URL": "http://127.0.0.1:7433", "TRACKER_TOKEN_FILE": "/run/tok"}[k]
	})
	if cfg.BaseURL != "http://127.0.0.1:7433" || cfg.TokenFile != "/run/tok" || cfg.Token != "" {
		t.Errorf("FromEnv = %+v, want the URL and token file", cfg)
	}
	cfg = FromEnv(func(k string) string {
		return map[string]string{"TRACKER_URL": "u", "TRACKER_TOKEN": "t"}[k]
	})
	if cfg.Token != "t" || cfg.TokenFile != "" {
		t.Errorf("FromEnv = %+v, want the token from TRACKER_TOKEN", cfg)
	}
}
