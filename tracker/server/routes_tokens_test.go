package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/store"
)

// tokenStub answers the token methods; any other method panics on the nil
// embedded interface, which a test shows as a failure.
type tokenStub struct {
	store.Store
	issue       func(ctx context.Context, actor string, scope api.Scope, expires *time.Time) (string, error)
	revokeActor func(ctx context.Context, actor string) (int, error)
	revokeID    func(ctx context.Context, id string) (bool, error)
}

func (s tokenStub) IssueTokenScoped(ctx context.Context, actor string, scope api.Scope, expires *time.Time) (string, error) {
	return s.issue(ctx, actor, scope, expires)
}
func (s tokenStub) RevokeActorTokens(ctx context.Context, actor string) (int, error) {
	return s.revokeActor(ctx, actor)
}
func (s tokenStub) RevokeTokenID(ctx context.Context, id string) (bool, error) {
	return s.revokeID(ctx, id)
}

func okTokens() tokenStub {
	return tokenStub{
		issue:       func(context.Context, string, api.Scope, *time.Time) (string, error) { return "trk_issued", nil },
		revokeActor: func(context.Context, string) (int, error) { return 0, nil },
		revokeID:    func(context.Context, string) (bool, error) { return false, nil },
	}
}

// Unit tier: httptest, stub store, no Postgres; milliseconds per case.
func TestAnAdminIssuesATokenForAnActorAndTheAnswerCarriesItOnce(t *testing.T) {
	t.Parallel()
	var actor string
	var scope api.Scope
	var expires *time.Time
	st := okTokens()
	st.issue = func(_ context.Context, a string, sc api.Scope, exp *time.Time) (string, error) {
		actor, scope, expires = a, sc, exp
		return "trk_issued_once", nil
	}
	ts := adminServer(t, st)

	code, body := adminCall(t, "POST", ts.URL+"/v1/tokens", `{"actor":"gastown/polecats/bullet"}`, true)
	if code != http.StatusCreated || actor != "gastown/polecats/bullet" || scope != api.ScopeSeat || expires != nil {
		t.Errorf("issue = %d %q; store got actor %q scope %q expires %v, want 201 for a seat token with no expiry", code, body, actor, scope, expires)
	}
	for _, want := range []string{`"token":"trk_issued_once"`, `"actor":"gastown/polecats/bullet"`, `"scope":"seat"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the issue answer %q does not carry %s", body, want)
		}
	}
	if n := strings.Count(body, "trk_issued_once"); n != 1 {
		t.Errorf("the token appears %d times in its own answer, want exactly once", n)
	}

	code, body = adminCall(t, "POST", ts.URL+"/v1/tokens", `{"actor":"town/daemon","scope":"admin"}`, true)
	if code != http.StatusCreated || actor != "town/daemon" || scope != api.ScopeAdmin || !strings.Contains(body, `"scope":"admin"`) {
		t.Errorf("issue with scope admin = %d %q; store got actor %q scope %q", code, body, actor, scope)
	}
}

func TestASeatTokenMayNeitherIssueNorRevoke(t *testing.T) {
	t.Parallel()
	called := false
	st := tokenStub{
		issue: func(context.Context, string, api.Scope, *time.Time) (string, error) {
			called = true
			return "trk_x", nil
		},
		revokeActor: func(context.Context, string) (int, error) {
			called = true
			return 1, nil
		},
		revokeID: func(context.Context, string) (bool, error) {
			called = true
			return true, nil
		},
	}
	ts := adminServer(t, st)
	for name, r := range map[string]struct{ path, body string }{
		"issue":        {"/v1/tokens", `{"actor":"gastown/polecats/bullet"}`},
		"revoke actor": {"/v1/tokens:revoke", `{"actor":"gastown/polecats/bullet"}`},
		"revoke id":    {"/v1/tokens:revoke", `{"token_id":"7f8d3c1e-0000-0000-0000-000000000000"}`},
	} {
		code, body := adminCallAs(t, "POST", ts.URL+r.path, r.body, "good-token")
		if e := decodeErr(t, body); code != http.StatusForbidden || e.Kind != api.KindForbidden {
			t.Errorf("%s with a seat token = %d %q, want 403 forbidden", name, code, body)
		}
	}
	if called {
		t.Error("a seat token reached the store on a token route")
	}
}

func TestRevokeEndsAnActorsTokensOrOneNamedToken(t *testing.T) {
	t.Parallel()
	var actor, id string
	st := okTokens()
	st.revokeActor = func(_ context.Context, a string) (int, error) { actor = a; return 3, nil }
	st.revokeID = func(_ context.Context, i string) (bool, error) { id = i; return true, nil }
	ts := adminServer(t, st)

	code, body := adminCall(t, "POST", ts.URL+"/v1/tokens:revoke", `{"actor":"gastown/polecats/bullet"}`, true)
	if code != http.StatusOK || !strings.Contains(body, `"revoked":3`) || actor != "gastown/polecats/bullet" {
		t.Errorf("revoke by actor = %d %q (store got %q), want 200 and revoked:3", code, body, actor)
	}
	code, body = adminCall(t, "POST", ts.URL+"/v1/tokens:revoke", `{"token_id":"tok-1"}`, true)
	if code != http.StatusOK || !strings.Contains(body, `"revoked":1`) || id != "tok-1" {
		t.Errorf("revoke by id = %d %q (store got %q), want 200 and revoked:1", code, body, id)
	}
}

func TestRevokingAnUnknownActorSucceedsAndAnUnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	st := okTokens()
	ts := adminServer(t, st)
	code, body := adminCall(t, "POST", ts.URL+"/v1/tokens:revoke", `{"actor":"gastown/polecats/nobody"}`, true)
	if code != http.StatusOK || !strings.Contains(body, `"revoked":0`) {
		t.Errorf("revoking an actor with no live token = %d %q, want 200 and revoked:0", code, body)
	}
	code, body = adminCall(t, "POST", ts.URL+"/v1/tokens:revoke", `{"token_id":"tok-gone"}`, true)
	if e := decodeErr(t, body); code != http.StatusNotFound || e.Kind != api.KindNotFound || !strings.Contains(e.Message, "tok-gone") {
		t.Errorf("revoking an id that names no live token = %d %q, want 404 naming it", code, body)
	}
}

func TestTokenRoutesRefuseBodiesThatNameNothingUsable(t *testing.T) {
	t.Parallel()
	ts := adminServer(t, okTokens())
	long := strings.Repeat("a", api.MaxTokenActorLen+1)
	for name, r := range map[string]struct{ path, body string }{
		"issue with no body":              {"/v1/tokens", ""},
		"issue with no actor":             {"/v1/tokens", `{}`},
		"issue with a blank actor":        {"/v1/tokens", `{"actor":"  "}`},
		"issue with a spaced actor":       {"/v1/tokens", `{"actor":"two seats"}`},
		"issue with a control character":  {"/v1/tokens", `{"actor":"seat\u001bone"}`},
		"issue with a long actor":         {"/v1/tokens", `{"actor":"` + long + `"}`},
		"issue with a scope no token has": {"/v1/tokens", `{"actor":"a","scope":"root"}`},
		"issue with an unknown field":     {"/v1/tokens", `{"actor":"a","reuse":true}`},
		"revoke naming both":              {"/v1/tokens:revoke", `{"actor":"a","token_id":"t"}`},
		"revoke naming neither":           {"/v1/tokens:revoke", `{}`},
		"revoke with a blank actor":       {"/v1/tokens:revoke", `{"actor":" "}`},
	} {
		code, body := adminCall(t, "POST", ts.URL+r.path, r.body, true)
		if e := decodeErr(t, body); code != http.StatusBadRequest || e.Kind != api.KindInvalid {
			t.Errorf("%s = %d %q, want 400 invalid", name, code, body)
		}
	}
}

func TestTokenRouteFailuresHideTheirCause(t *testing.T) {
	t.Parallel()
	st := okTokens()
	cause := errors.New("store: issue token: dial tcp 10.1.2.3:5432: password authentication failed")
	st.issue = func(context.Context, string, api.Scope, *time.Time) (string, error) { return "", cause }
	st.revokeActor = func(context.Context, string) (int, error) { return 0, cause }
	st.revokeID = func(context.Context, string) (bool, error) { return false, cause }
	ts := adminServer(t, st)
	for _, r := range []struct{ path, body string }{
		{"/v1/tokens", `{"actor":"a"}`},
		{"/v1/tokens:revoke", `{"actor":"a"}`},
		{"/v1/tokens:revoke", `{"token_id":"t"}`},
	} {
		code, body := adminCall(t, "POST", ts.URL+r.path, r.body, true)
		if e := decodeErr(t, body); code != http.StatusServiceUnavailable || e.Kind != api.KindUnavailable {
			t.Errorf("%s with a failing store = %d %q, want 503 unavailable", r.path, code, body)
		}
		for _, leak := range []string{"10.1.2.3", "password", "5432"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s leaks its cause: %q", r.path, body)
			}
		}
	}
}

// An issued token is a bearer credential: it may appear in the answer that
// carries it and nowhere else the server records. The metrics count the actor
// and the route, never the body; the log holds failures, which name the rule
// and not the value.
func TestAnIssuedTokenReachesNeitherTheLogNorTheMetrics(t *testing.T) {
	t.Parallel()
	const token = "trk_5ecretvalue5ecretvalue5ecretvalue5e"
	st := okTokens()
	st.issue = func(context.Context, string, api.Scope, *time.Time) (string, error) { return token, nil }
	log := &recLog{}
	s := New(Config{Auth: fakeAuth{"admin-token": "town/daemon"}, Health: fakeHealth{}, SchemaLevel: 3, Store: st, Log: log})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	if code, body := adminCall(t, "POST", ts.URL+"/v1/tokens", `{"actor":"gastown/polecats/bullet"}`, true); code != http.StatusCreated || !strings.Contains(body, token) {
		t.Fatalf("issue = %d %q, want 201 carrying the token", code, body)
	}
	// A refused issue runs the error path, the one place a value could reach a
	// log line if the server put a body or a token in it.
	if code, _ := adminCall(t, "POST", ts.URL+"/v1/tokens", `{"actor":""}`, true); code != http.StatusBadRequest {
		t.Fatalf("an empty actor = %d, want 400", code)
	}
	if logged := log.all(); strings.Contains(logged, token) || strings.Contains(logged, "trk_") {
		t.Errorf("the log holds the token: %q", logged)
	}
	code, body := adminCall(t, "GET", ts.URL+"/metrics", "", false)
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", code)
	}
	if strings.Contains(body, token) || strings.Contains(body, "gastown/polecats/bullet") {
		t.Errorf("the metrics hold the token or the whole body: the value must not be a label")
	}
	if !strings.Contains(body, "town/daemon") {
		t.Errorf("the metrics do not count the actor of the request: %q", body)
	}
}

func TestClientTokenMethodsAgreeWithTheRoutes(t *testing.T) {
	t.Parallel()
	var revokedActor string
	st := okTokens()
	st.issue = func(_ context.Context, actor string, scope api.Scope, _ *time.Time) (string, error) {
		return "trk_for_" + actor + "_" + string(scope), nil
	}
	st.revokeActor = func(_ context.Context, actor string) (int, error) { revokedActor = actor; return 2, nil }
	st.revokeID = func(_ context.Context, id string) (bool, error) { return id == "tok-live", nil }
	ts := adminServer(t, st)
	// The server's own client, so ts.Close closes only connections this test
	// opened, not http.DefaultTransport's shared with its siblings (gt-djzxe).
	hc := ts.Client()
	hc.Timeout = adminHTTP.Timeout
	admin, err := client.New(client.Config{BaseURL: ts.URL, Token: "admin-token", SchemaLevel: 3, HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := admin.IssueToken(ctx, "gastown/polecats/bullet", api.ScopeSeat)
	if err != nil || got.Actor != "gastown/polecats/bullet" || got.Scope != api.ScopeSeat || got.Token != "trk_for_gastown/polecats/bullet_seat" {
		t.Errorf("IssueToken = %+v, %v", got, err)
	}
	if n, err := admin.RevokeActorTokens(ctx, "gastown/polecats/bullet"); err != nil || n != 2 || revokedActor != "gastown/polecats/bullet" {
		t.Errorf("RevokeActorTokens = %d, %v (store got %q)", n, err, revokedActor)
	}
	if err := admin.RevokeTokenID(ctx, "tok-gone"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("RevokeTokenID of an id that names no live token = %v, want api.ErrNotFound through the client", err)
	}
	if err := admin.RevokeTokenID(ctx, "tok-live"); err != nil {
		t.Errorf("RevokeTokenID of a live token = %v, want no error", err)
	}
}
