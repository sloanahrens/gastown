package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/store"
)

// adminStub answers the three admin methods; any other method panics on the
// nil embedded interface, which a test shows as a failure.
type adminStub struct {
	store.Store
	stats func(ctx context.Context, rig string) (api.Stats, error)
	get   func(ctx context.Context, key string) (string, bool, error)
	set   func(ctx context.Context, key, value string) error
}

func (s adminStub) Stats(ctx context.Context, rig string) (api.Stats, error) {
	return s.stats(ctx, rig)
}
func (s adminStub) ConfigGet(ctx context.Context, k string) (string, bool, error) {
	return s.get(ctx, k)
}
func (s adminStub) ConfigSet(ctx context.Context, k, v string) error { return s.set(ctx, k, v) }

// adminHTTP owns its transport, with keep-alives off, because httptest's
// Server.Close closes http.DefaultTransport's idle connections: on that shared
// transport a parallel test's cleanup can close the connection a sibling's
// request is using, and a PUT is not retried (gt-djzxe).
var adminHTTP = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{DisableKeepAlives: true},
}

func adminServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	return adminServerAs(t, st, nil)
}

// adminServerAs is adminServer with a logger, so a test can read the lines a
// route wrote.
func adminServerAs(t *testing.T, st store.Store, log Logger) *httptest.Server {
	t.Helper()
	s := New(Config{Auth: fakeAuth{"good-token": "town/crew/dipper", "admin-token": "town/daemon"}, Health: fakeHealth{}, SchemaLevel: 3, Store: st, Log: log})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func adminCall(t *testing.T, method, url, body string, token bool) (int, string) {
	t.Helper()
	tok := ""
	if token {
		tok = "admin-token"
	}
	return adminCallAs(t, method, url, body, tok)
}

// adminCallAs sends a request with the bearer token tok ("" for none).
func adminCallAs(t *testing.T, method, url, body, tok string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set(api.SchemaHeader, "3")
	resp, err := adminHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func okAdmin() adminStub {
	return adminStub{
		stats: func(context.Context, string) (api.Stats, error) { return api.Stats{}, nil },
		get:   func(context.Context, string) (string, bool, error) { return "", false, nil },
		set:   func(context.Context, string, string) error { return nil },
	}
}

// Unit tier: httptest, stub store, no Postgres; milliseconds per case.
func TestStatsPassesTheRigAndAnswersAnEmptyMapNotNull(t *testing.T) {
	t.Parallel()
	var rigs []string
	st := okAdmin()
	st.stats = func(_ context.Context, rig string) (api.Stats, error) {
		rigs = append(rigs, rig)
		return api.Stats{Total: 0}, nil
	}
	ts := adminServer(t, st)
	code, body := adminCall(t, "GET", ts.URL+"/v1/stats?rig=gt", "", true)
	if code != 200 || !strings.Contains(body, `"by_status":{}`) || !strings.Contains(body, `"total":0`) {
		t.Errorf("stats = %d %q, want 200 and an empty by_status object, not null", code, body)
	}
	adminCall(t, "GET", ts.URL+"/v1/stats", "", true)
	if len(rigs) != 2 || rigs[0] != "gt" || rigs[1] != "" {
		t.Errorf("rigs reaching the store = %q, want gt, then empty for every rig", rigs)
	}
}

func TestConfigGetAndSetPassTheKeyAndValue(t *testing.T) {
	t.Parallel()
	var gotKey, setKey, setVal string
	st := okAdmin()
	st.get = func(_ context.Context, k string) (string, bool, error) { gotKey = k; return "agent,rig", true, nil }
	st.set = func(_ context.Context, k, v string) error { setKey, setVal = k, v; return nil }
	ts := adminServer(t, st)
	code, body := adminCall(t, "GET", ts.URL+"/v1/config/types.custom", "", true)
	if code != 200 || !strings.Contains(body, `"value":"agent,rig"`) || !strings.Contains(body, `"set":true`) || gotKey != "types.custom" {
		t.Errorf("config get = %d %q (key %q), want the value and set:true", code, body, gotKey)
	}
	if code, body := adminCall(t, "PUT", ts.URL+"/v1/config/types.custom", `{"value":"agent"}`, true); code != 204 || body != "" || setKey != "types.custom" || setVal != "agent" {
		t.Errorf("config set = %d %q; store got %q = %q, want 204", code, body, setKey, setVal)
	}
	if code, _ := adminCall(t, "PUT", ts.URL+"/v1/config/blank", `{"value":""}`, true); code != 204 || setKey != "blank" || setVal != "" {
		t.Errorf("setting an empty value = %d (%q = %q), want 204 and the empty value delivered", code, setKey, setVal)
	}
}

func TestConfigGetOfAnUnsetKeyIsSetFalse(t *testing.T) {
	t.Parallel()
	ts := adminServer(t, okAdmin())
	code, body := adminCall(t, "GET", ts.URL+"/v1/config/nope", "", true)
	if code != 200 || !strings.Contains(body, `"set":false`) {
		t.Errorf("unset key = %d %q, want 200 and set:false (not a 404)", code, body)
	}
}

func TestAdminRoutesMapStoreErrorsAndHideInternalOnes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   api.Kind
		leak   string
	}{
		"invalid":  {api.Invalid("a configuration key is required"), 400, api.KindInvalid, ""},
		"internal": {context.DeadlineExceeded, 503, api.KindUnavailable, "deadline"},
	} {
		st := adminStub{
			stats: func(context.Context, string) (api.Stats, error) { return api.Stats{}, tc.err },
			get:   func(context.Context, string) (string, bool, error) { return "", false, tc.err },
			set:   func(context.Context, string, string) error { return tc.err },
		}
		ts := adminServer(t, st)
		for _, r := range []struct{ method, path, body string }{
			{"GET", "/v1/stats", ""}, {"GET", "/v1/config/k", ""}, {"PUT", "/v1/config/k", `{"value":"v"}`},
		} {
			code, body := adminCall(t, r.method, ts.URL+r.path, r.body, true)
			if e := decodeErr(t, body); code != tc.status || e.Kind != tc.kind {
				t.Errorf("%s %s %s: %d %q, want %d %s", name, r.method, r.path, code, body, tc.status, tc.kind)
			}
			if tc.leak != "" && strings.Contains(body, tc.leak) {
				t.Errorf("%s %s: the response leaks the cause: %q", name, r.path, body)
			}
		}
	}
}

func TestConfigSetRefusesBadBodiesAndEveryAdminRouteNeedsAToken(t *testing.T) {
	t.Parallel()
	ts := adminServer(t, okAdmin())
	for name, body := range map[string]string{"empty": "", "unknown field": `{"surprise":1}`, "not json": `{`, "trailing data": `{}{}`} {
		code, resp := adminCall(t, "PUT", ts.URL+"/v1/config/k", body, true)
		if e := decodeErr(t, resp); code != 400 || e.Kind != api.KindInvalid {
			t.Errorf("PUT %s = %d %q, want 400 invalid", name, code, resp)
		}
	}
	for _, r := range []struct{ method, path, body string }{{"GET", "/v1/stats", ""}, {"GET", "/v1/config/k", ""}, {"PUT", "/v1/config/k", `{"value":"v"}`}} {
		if code, _ := adminCall(t, r.method, ts.URL+r.path, r.body, false); code != 401 {
			t.Errorf("%s %s without a token = %d, want 401", r.method, r.path, code)
		}
		req, _ := http.NewRequest(r.method, ts.URL+r.path, strings.NewReader(r.body))
		req.Header.Set("Authorization", "Bearer wrong-token")
		req.Header.Set(api.SchemaHeader, "3")
		resp, err := adminHTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s %s with an unknown token = %d, want 401", r.method, r.path, resp.StatusCode)
		}
	}
}

func TestClientAdminAgreesWithTheRoutes(t *testing.T) {
	t.Parallel()
	vals := map[string]string{}
	st := okAdmin()
	st.stats = func(_ context.Context, rig string) (api.Stats, error) {
		return api.Stats{Total: 3, ByStatus: map[string]int{"open": 2, "closed": 1}}, nil
	}
	st.get = func(_ context.Context, k string) (string, bool, error) { v, ok := vals[k]; return v, ok, nil }
	st.set = func(_ context.Context, k, v string) error { vals[k] = v; return nil }
	ts := adminServer(t, st)
	// The server's own client, so ts.Close closes only connections this test
	// opened, not http.DefaultTransport's shared with its siblings (gt-djzxe).
	// ts.Client() has no timeout of its own; restore the bound adminHTTP put
	// on each request, so this client is cut off at 5 s and not the client's
	// 10 s default.
	hc := ts.Client()
	hc.Timeout = adminHTTP.Timeout
	c, err := client.New(client.Config{BaseURL: ts.URL, Token: "good-token", SchemaLevel: 3, HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(client.Config{BaseURL: ts.URL, Token: "admin-token", SchemaLevel: 3, HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got, err := c.Stats(ctx, "gt"); err != nil || got.Total != 3 || got.ByStatus["closed"] != 1 {
		t.Errorf("Stats = %+v, %v", got, err)
	}
	if err := c.ConfigSet(ctx, "types.custom", "x"); !errors.Is(err, api.ErrForbidden) {
		t.Errorf("a seat client setting config = %v, want api.ErrForbidden through the client", err)
	}
	if err := admin.ConfigSet(ctx, "types.custom", "agent,rig"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := c.ConfigGet(ctx, "types.custom"); err != nil || !ok || v != "agent,rig" {
		t.Errorf("ConfigGet = %q, %v, %v", v, ok, err)
	}
	if v, ok, err := c.ConfigGet(ctx, "missing"); err != nil || ok || v != "" {
		t.Errorf("ConfigGet(missing) = %q, %v, %v; want empty and unset", v, ok, err)
	}
}

func TestOnlyAnAdminTokenMayWriteConfigurationAndASeatMayStillReadIt(t *testing.T) {
	t.Parallel()
	var set []string
	st := okAdmin()
	st.set = func(_ context.Context, k, v string) error { set = append(set, k+"="+v); return nil }
	ts := adminServer(t, st)
	code, body := adminCallAs(t, "PUT", ts.URL+"/v1/config/k", `{"value":"v"}`, "good-token")
	if e := decodeErr(t, body); code != 403 || e.Kind != api.KindForbidden {
		t.Errorf("a seat token writing config = %d %q, want 403 forbidden", code, body)
	}
	if len(set) != 0 {
		t.Errorf("the store was written by a seat token: %v", set)
	}
	if code, _ := adminCallAs(t, "PUT", ts.URL+"/v1/config/k", `{"value":"v"}`, "admin-token"); code != 204 || len(set) != 1 || set[0] != "k=v" {
		t.Errorf("an admin token writing config = %d (store %v), want 204 and the write", code, set)
	}
	for _, path := range []string{"/v1/config/k", "/v1/stats"} {
		if code, _ := adminCallAs(t, "GET", ts.URL+path, "", "good-token"); code != 200 {
			t.Errorf("a seat token reading %s = %d, want 200", path, code)
		}
	}
}

func TestAnUnknownTokenIsStill401NotForbidden(t *testing.T) {
	t.Parallel()
	ts := adminServer(t, okAdmin())
	if code, _ := adminCallAs(t, "PUT", ts.URL+"/v1/config/k", `{"value":"v"}`, "nope"); code != 401 {
		t.Errorf("an unknown token on an admin route = %d, want 401", code)
	}
}

// A sibling test's cleanup calls httptest.Server.Close, which calls
// http.DefaultTransport's CloseIdleConnections -- closing every idle connection
// the shared client pooled, including one this test's next PUT would be handed,
// and a PUT that loses its connection is not retried (gt-djzxe). So adminHTTP
// must hold no connection between requests: after a sibling's cleanup it still
// opens a fresh one for each request rather than reusing a pooled connection.
func TestASiblingServersCleanupCannotBreakTheAdminClient(t *testing.T) {
	t.Parallel()
	var conns atomic.Int64
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)

	sibling := adminServer(t, okAdmin())
	put := func(i int) {
		t.Helper()
		if code, body := adminCallAs(t, "PUT", ts.URL+"/v1/config/k", `{"value":"v"}`, "admin-token"); code != http.StatusNoContent {
			t.Fatalf("request %d = %d %q, want 204", i, code, body)
		}
	}
	put(0)
	sibling.Close() // exactly what the sibling test's t.Cleanup(ts.Close) does
	put(1)
	put(2)
	// One connection per request: the last request was not handed a connection
	// the sibling's cleanup could have closed under it.
	if got := conns.Load(); got != 3 {
		t.Errorf("three requests after a sibling's cleanup opened %d connections, want 3 (no connection survives the cleanup for reuse)", got)
	}
}

func TestRequireAdminRefusesAContextWithNoIdentityAndAnySeat(t *testing.T) {
	t.Parallel() // unit tier: the middleware alone, no server
	called := false
	h := requireAdmin(func(http.ResponseWriter, *http.Request) { called = true })
	for name, ctx := range map[string]context.Context{
		"no identity": context.Background(),
		"seat":        api.WithIdentity(context.Background(), api.Identity{Actor: "a", Scope: api.ScopeSeat}),
		"junk scope":  api.WithIdentity(context.Background(), api.Identity{Actor: "a", Scope: "root"}),
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("PUT", "/v1/config/k", nil).WithContext(ctx))
		if rec.Code != 403 || called {
			t.Errorf("%s: status %d, handler called %v; want 403 and the handler not called", name, rec.Code, called)
		}
	}
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("PUT", "/v1/config/k", nil).WithContext(api.WithIdentity(context.Background(), api.Identity{Actor: "d", Scope: api.ScopeAdmin})))
	if !called {
		t.Error("an admin identity did not reach the handler")
	}
}
