package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

func post(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

type sample struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func TestDecodeJSONAcceptsAWellFormedBody(t *testing.T) {
	t.Parallel() // unit tier: pure
	var v sample
	if err := decodeJSON(post(`{"name":"a","n":3}`), &v); err != nil || v.Name != "a" || v.N != 3 {
		t.Errorf("decodeJSON = %+v, %v; want name a, n 3", v, err)
	}
}

func TestDecodeJSONRefusesBadBodiesAsInvalidAndSaysWhy(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ body, why string }{
		"empty":            {``, "required"},
		"blank":            {"  \n", "required"},
		"not json":         {`{nope`, "not valid"},
		"unknown field":    {`{"name":"a","surprise":1}`, "surprise"},
		"trailing data":    {`{"name":"a"}{"name":"b"}`, "after the first"},
		"wrong field type": {`{"name":5}`, "not valid"},
		"too large":        {`{"name":"` + strings.Repeat("x", maxBodyBytes) + `"}`, "larger than"},
	} {
		var v sample
		err := decodeJSON(post(tc.body), &v)
		ae, ok := err.(*api.Error)
		if !ok || ae.Kind != api.KindInvalid || !strings.Contains(ae.Message, tc.why) {
			t.Errorf("%s: error = %v, want an api invalid error that says %q", name, err, tc.why)
		}
	}
}

// The actor reaches store writes through the context, from one place.
func TestTheAuthenticatedActorIsOnTheRequestContextForStores(t *testing.T) {
	t.Parallel()
	var seen string
	s := New(Config{Auth: fakeAuth{"good-token": "town/crew/dipper"}, Health: fakeHealth{}, SchemaLevel: 3})
	s.routes.Handle("GET /v1/probe-actor", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = api.ActorFrom(r.Context())
		w.WriteHeader(204)
	}))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	code, _ := get(t, ts.URL+"/v1/probe-actor", map[string]string{"Authorization": "Bearer good-token", api.SchemaHeader: "3"})
	if code != 204 || seen != "town/crew/dipper" {
		t.Errorf("status %d, api.ActorFrom = %q; want 204 and the token's actor", code, seen)
	}
}
