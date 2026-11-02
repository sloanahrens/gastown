package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/storetest"
)

func kvConfig(st store.Store) Config {
	return Config{Auth: fakeAuth{"good-token": "town/crew/dipper", "admin-token": "town/daemon"}, Health: fakeHealth{}, SchemaLevel: 3, Store: st}
}

// kvServer starts a real server over the fake store and returns it with a seat
// client and an admin client for it.
func kvServer(t *testing.T) (ts *httptest.Server, seat, admin *client.Client) {
	t.Helper()
	ts = httptest.NewServer(New(kvConfig(storetest.NewFake())).Handler())
	t.Cleanup(ts.Close)
	// The server's own client, bounded like adminHTTP, so ts.Close closes only
	// the connections this server opened (gt-djzxe).
	hc := ts.Client()
	hc.Timeout = adminHTTP.Timeout
	seat, err := client.New(client.Config{BaseURL: ts.URL, Token: "good-token", SchemaLevel: 3, HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	admin, err = client.New(client.Config{BaseURL: ts.URL, Token: "admin-token", SchemaLevel: 3, HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	return ts, seat, admin
}

// Unit tier: the real client against the real routes over the fake store.
func TestClientKVAgreesWithTheRoutes(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		_, seat, admin := kvServer(t)
		ctx := context.Background()
		if err := admin.KVSet(ctx, "types.custom", "agent,rig"); err != nil {
			t.Fatalf("KVSet = %v, want nil", err)
		}
		v, ok, err := seat.KVGet(ctx, "types.custom")
		if err != nil || !ok || v != "agent,rig" {
			t.Errorf("KVGet = %q, %v, %v; want the value set and read by a seat", v, ok, err)
		}
	})

	t.Run("not set", func(t *testing.T) {
		t.Parallel()
		_, seat, _ := kvServer(t)
		v, ok, err := seat.KVGet(context.Background(), "never.set")
		if err != nil || ok || v != "" {
			t.Errorf("KVGet(unset) = %q, %v, %v; want empty and not set, never a 404", v, ok, err)
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		t.Parallel()
		_, seat, admin := kvServer(t)
		ctx := context.Background()
		for _, v := range []string{"first", "second"} {
			if err := admin.KVSet(ctx, "k", v); err != nil {
				t.Fatalf("KVSet(%q) = %v", v, err)
			}
		}
		if v, ok, err := seat.KVGet(ctx, "k"); err != nil || !ok || v != "second" {
			t.Errorf("KVGet after overwrite = %q, %v, %v; want second", v, ok, err)
		}
	})

	t.Run("list", func(t *testing.T) {
		t.Parallel()
		_, seat, admin := kvServer(t)
		ctx := context.Background()
		want := map[string]string{"a": "1", "blank": "", "b": "2"}
		for k, v := range want {
			if err := admin.KVSet(ctx, k, v); err != nil {
				t.Fatalf("KVSet(%q) = %v", k, err)
			}
		}
		got, err := seat.KVList(ctx)
		if err != nil || len(got) != len(want) {
			t.Fatalf("KVList = %v, %v; want every pair", got, err)
		}
		for k, v := range want {
			if gv, ok := got[k]; !ok || gv != v {
				t.Errorf("KVList[%q] = %q, %v; want %q, a blank one included", k, gv, ok, v)
			}
		}
	})

	t.Run("list of an empty table", func(t *testing.T) {
		t.Parallel()
		_, seat, _ := kvServer(t)
		got, err := seat.KVList(context.Background())
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("KVList of an empty table = %v, %v; want an empty, non-nil map", got, err)
		}
	})

	t.Run("empty key", func(t *testing.T) {
		t.Parallel()
		_, seat, admin := kvServer(t)
		ctx := context.Background()
		// A key is one non-empty path segment, so /v1/kv/ never reaches a
		// handler: the mux refuses it as it refuses /v1/config/, and the
		// client reports both as api.ErrInvalid, the config routes' error.
		if _, _, err := seat.KVGet(ctx, ""); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("KVGet(empty key) = %v, want api.ErrInvalid like the config route", err)
		}
		if err := admin.KVSet(ctx, "", "x"); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("KVSet(empty key) = %v, want api.ErrInvalid like the config route", err)
		}
		if _, _, err := seat.ConfigGet(ctx, ""); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("the config route no longer refuses an empty key with api.ErrInvalid: %v", err)
		}
		if got, err := seat.KVList(ctx); err != nil || len(got) != 0 {
			t.Errorf("after an empty-key write the store holds %v, %v; want nothing", got, err)
		}
	})

	t.Run("wrong scope", func(t *testing.T) {
		t.Parallel()
		_, seat, admin := kvServer(t)
		ctx := context.Background()
		if err := admin.KVSet(ctx, "k", "admin"); err != nil {
			t.Fatal(err)
		}
		if err := seat.KVSet(ctx, "k", "seat"); !errors.Is(err, api.ErrForbidden) {
			t.Errorf("a seat token writing kv = %v, want api.ErrForbidden", err)
		}
		if v, ok, err := seat.KVGet(ctx, "k"); err != nil || !ok || v != "admin" {
			t.Errorf("after a refused seat write KVGet = %q, %v, %v; want the admin value kept", v, ok, err)
		}
	})

	t.Run("bad body", func(t *testing.T) {
		t.Parallel()
		ts, _, _ := kvServer(t)
		for name, body := range map[string]string{"empty": "", "unknown field": `{"surprise":1}`, "not json": `{`} {
			code, resp := adminCallAs(t, "PUT", ts.URL+"/v1/kv/k", body, "admin-token")
			if e := decodeErr(t, resp); code != http.StatusBadRequest || e.Kind != api.KindInvalid {
				t.Errorf("PUT %s = %d %q, want 400 invalid", name, code, resp)
			}
		}
	})
}

// kvStub fails the three kv methods with one error; any other method panics on
// the nil embedded interface, which a test shows as a failure.
type kvStub struct {
	store.Store
	err error
}

func (s kvStub) KVGet(context.Context, string) (string, bool, error) { return "", false, s.err }
func (s kvStub) KVSet(context.Context, string, string) error         { return s.err }
func (s kvStub) KVList(context.Context) (map[string]string, error) {
	return nil, s.err
}

// The kv routes map a store error as the config routes do and never echo the
// cause of one they do not know.
func TestKVRoutesMapStoreErrors(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   api.Kind
		leak   string
	}{
		"invalid":  {api.Invalid("a key is required"), http.StatusBadRequest, api.KindInvalid, ""},
		"internal": {context.DeadlineExceeded, http.StatusServiceUnavailable, api.KindUnavailable, "deadline"},
	} {
		ts := httptest.NewServer(New(kvConfig(kvStub{err: tc.err})).Handler())
		t.Cleanup(ts.Close)
		for _, r := range []struct{ method, path, body string }{
			{"GET", "/v1/kv", ""}, {"GET", "/v1/kv/k", ""}, {"PUT", "/v1/kv/k", `{"value":"v"}`},
		} {
			code, body := adminCallAs(t, r.method, ts.URL+r.path, r.body, "admin-token")
			if e := decodeErr(t, body); code != tc.status || e.Kind != tc.kind {
				t.Errorf("%s %s %s: %d %q, want %d %s", name, r.method, r.path, code, body, tc.status, tc.kind)
			}
			if tc.leak != "" && strings.Contains(body, tc.leak) {
				t.Errorf("%s %s: the answer leaks the cause: %q", name, r.path, body)
			}
		}
	}
}

// A server that answers no items still yields a map the caller can use, never
// a nil one.
func TestClientKVListNormalizesAnEmptyAnswer(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":null}`))
	}))
	t.Cleanup(ts.Close)
	c, err := client.New(client.Config{BaseURL: ts.URL, Token: "t", SchemaLevel: 3, HTTP: ts.Client()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.KVList(context.Background())
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("KVList of a null answer = %v, %v; want an empty, non-nil map", got, err)
	}
}

// A store that answers no pairs with a nil map still gets an empty object on
// the wire, as stats answers an empty by_status.
func TestKVListAnswersAnEmptyObjectForANilMap(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(New(kvConfig(kvStub{})).Handler())
	t.Cleanup(ts.Close)
	code, body := adminCallAs(t, "GET", ts.URL+"/v1/kv", "", "admin-token")
	if code != http.StatusOK || !strings.Contains(body, `"items":{}`) {
		t.Errorf("kv list = %d %q, want 200 and an empty items object, not null", code, body)
	}
}
