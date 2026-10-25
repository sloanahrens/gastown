package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

var adminHTTP = &http.Client{Timeout: 5 * time.Second}

func adminServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	s := New(Config{Auth: fakeAuth{"good-token": "town/crew/dipper"}, Health: fakeHealth{}, SchemaLevel: 3, Store: st})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func adminCall(t *testing.T, method, url, body string, token bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token {
		req.Header.Set("Authorization", "Bearer good-token")
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
	c, err := client.New(client.Config{BaseURL: ts.URL, Token: "good-token", SchemaLevel: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got, err := c.Stats(ctx, "gt"); err != nil || got.Total != 3 || got.ByStatus["closed"] != 1 {
		t.Errorf("Stats = %+v, %v", got, err)
	}
	if err := c.ConfigSet(ctx, "types.custom", "agent,rig"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := c.ConfigGet(ctx, "types.custom"); err != nil || !ok || v != "agent,rig" {
		t.Errorf("ConfigGet = %q, %v, %v", v, ok, err)
	}
	if v, ok, err := c.ConfigGet(ctx, "missing"); err != nil || ok || v != "" {
		t.Errorf("ConfigGet(missing) = %q, %v, %v; want empty and unset", v, ok, err)
	}
}
