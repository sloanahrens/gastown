package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// writeStub records what each write method received; any other method panics
// on the nil embedded interface, which a test shows as a failure.
type writeStub struct {
	store.Store
	create func(ctx context.Context, p api.CreateParams) (api.Issue, error)
	update func(ctx context.Context, id string, patch api.UpdatePatch, g api.Guard) error
	close  func(ctx context.Context, ids []string, reason string, force bool) error
	del    func(ctx context.Context, ids []string) error
}

func (s writeStub) CreateIssue(ctx context.Context, p api.CreateParams) (api.Issue, error) {
	return s.create(ctx, p)
}

func (s writeStub) UpdateIssue(ctx context.Context, id string, p api.UpdatePatch, g api.Guard) error {
	return s.update(ctx, id, p, g)
}

func (s writeStub) CloseIssues(ctx context.Context, ids []string, reason string, force bool) error {
	return s.close(ctx, ids, reason, force)
}

func (s writeStub) DeleteIssues(ctx context.Context, ids []string) error { return s.del(ctx, ids) }

func writeServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	s := New(Config{Auth: fakeAuth{"good-token": "town/crew/dipper"}, Health: fakeHealth{}, SchemaLevel: 3, Store: st})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func writeCall(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer good-token")
	req.Header.Set(api.SchemaHeader, "3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const tokenActor = "town/crew/dipper"

func okStub() writeStub {
	return writeStub{
		create: func(context.Context, api.CreateParams) (api.Issue, error) { return api.Issue{ID: "gt-new"}, nil },
		update: func(context.Context, string, api.UpdatePatch, api.Guard) error { return nil },
		close:  func(context.Context, []string, string, bool) error { return nil },
		del:    func(context.Context, []string) error { return nil },
	}
}

// Unit tier: httptest, stub store, no Postgres; milliseconds per case.
func TestCreateUsesTheTokensActorAnswers201AndPassesTheParams(t *testing.T) {
	t.Parallel()
	var got api.CreateParams
	var ctxActor string
	st := okStub()
	st.create = func(ctx context.Context, p api.CreateParams) (api.Issue, error) {
		got, ctxActor = p, api.ActorFrom(ctx)
		return api.Issue{ID: "gt-new", Title: p.Title}, nil
	}
	ts := writeServer(t, st)
	code, body := writeCall(t, "POST", ts.URL+"/v1/issues", `{"title":"t","rig":"gt","priority":2,"actor":"mallory","labels":["x"]}`)
	if code != 201 || !strings.Contains(body, `"id":"gt-new"`) {
		t.Fatalf("create = %d %q, want 201 with the issue", code, body)
	}
	if got.Actor != tokenActor || ctxActor != tokenActor {
		t.Errorf("actor = %q (params) / %q (context), want the token's %q and not the body's mallory", got.Actor, ctxActor, tokenActor)
	}
	if got.Title != "t" || got.Rig != "gt" || got.Priority != 2 || len(got.Labels) != 1 {
		t.Errorf("params = %+v, want the body passed through", got)
	}
}

func TestUpdateUsesTheTokensActorTheIDAndTheGuardAnswers204(t *testing.T) {
	t.Parallel()
	var id string
	var patch api.UpdatePatch
	var guard api.Guard
	var ctxActor string
	st := okStub()
	st.update = func(ctx context.Context, i string, p api.UpdatePatch, g api.Guard) error {
		id, patch, guard, ctxActor = i, p, g, api.ActorFrom(ctx)
		return nil
	}
	ts := writeServer(t, st)
	code, body := writeCall(t, "PATCH", ts.URL+"/v1/issues/gt-abc", `{"patch":{"title":"new","actor":"mallory"},"guard":{"if_assignee":"me"}}`)
	if code != 204 || body != "" {
		t.Fatalf("update = %d %q, want 204 and no body", code, body)
	}
	if id != "gt-abc" || patch.Actor != tokenActor || ctxActor != tokenActor || patch.Title == nil || *patch.Title != "new" || guard.IfAssignee == nil || *guard.IfAssignee != "me" {
		t.Errorf("got id=%q patch=%+v guard=%+v ctxActor=%q", id, patch, guard, ctxActor)
	}
}

func TestCloseAndDeletePassTheirArgumentsAndAnswer204(t *testing.T) {
	t.Parallel()
	var closeIDs, delIDs []string
	var reason string
	var force bool
	st := okStub()
	st.close = func(_ context.Context, ids []string, r string, f bool) error {
		closeIDs, reason, force = ids, r, f
		return nil
	}
	st.del = func(_ context.Context, ids []string) error { delIDs = ids; return nil }
	ts := writeServer(t, st)
	if code, body := writeCall(t, "POST", ts.URL+"/v1/issues:close", `{"ids":["gt-a","gt-b"],"reason":"done","force":true}`); code != 204 || body != "" {
		t.Errorf("close = %d %q, want 204", code, body)
	}
	if code, body := writeCall(t, "POST", ts.URL+"/v1/issues:delete", `{"ids":["gt-c"]}`); code != 204 || body != "" {
		t.Errorf("delete = %d %q, want 204", code, body)
	}
	if len(closeIDs) != 2 || reason != "done" || !force || len(delIDs) != 1 || delIDs[0] != "gt-c" {
		t.Errorf("close got %v %q %v, delete got %v", closeIDs, reason, force, delIDs)
	}
}

func TestWriteRoutesMapStoreErrorsToStatusAndKind(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   api.Kind
		leak   string
	}{
		"guard failed": {api.GuardFailed("assignee changed"), 409, api.KindGuardFailed, ""},
		"not found":    {api.NotFound("gt-x"), 404, api.KindNotFound, ""},
		"conflict":     {api.Conflict("open children"), 409, api.KindConflict, ""},
		"invalid":      {api.Invalid("empty title"), 400, api.KindInvalid, ""},
		"internal":     {context.DeadlineExceeded, 503, api.KindUnavailable, "deadline"},
	} {
		fail := func() error { return tc.err }
		st := writeStub{
			create: func(context.Context, api.CreateParams) (api.Issue, error) { return api.Issue{}, fail() },
			update: func(context.Context, string, api.UpdatePatch, api.Guard) error { return fail() },
			close:  func(context.Context, []string, string, bool) error { return fail() },
			del:    func(context.Context, []string) error { return fail() },
		}
		ts := writeServer(t, st)
		for _, r := range []struct{ method, path, body string }{
			{"POST", "/v1/issues", `{"title":"t"}`}, {"PATCH", "/v1/issues/gt-a", `{"patch":{},"guard":{}}`},
			{"POST", "/v1/issues:close", `{"ids":["gt-a"]}`}, {"POST", "/v1/issues:delete", `{"ids":["gt-a"]}`},
		} {
			code, body := writeCall(t, r.method, ts.URL+r.path, r.body)
			if e := decodeErr(t, body); code != tc.status || e.Kind != tc.kind {
				t.Errorf("%s %s %s: %d %q, want %d %s", name, r.method, r.path, code, body, tc.status, tc.kind)
			}
			if tc.leak != "" && strings.Contains(body, tc.leak) {
				t.Errorf("%s %s: the response leaks the cause: %q", name, r.path, body)
			}
		}
	}
}

func TestWriteRoutesRefuseEmptyAndUnknownFieldBodies(t *testing.T) {
	t.Parallel()
	ts := writeServer(t, writeStub{})
	for _, r := range []struct{ method, path string }{
		{"POST", "/v1/issues"}, {"PATCH", "/v1/issues/gt-a"}, {"POST", "/v1/issues:close"}, {"POST", "/v1/issues:delete"},
	} {
		for name, body := range map[string]string{"empty": "", "unknown field": `{"surprise":1}`, "not json": `{`, "trailing data": `{}{}`} {
			code, resp := writeCall(t, r.method, ts.URL+r.path, body)
			if e := decodeErr(t, resp); code != 400 || e.Kind != api.KindInvalid {
				t.Errorf("%s %s %s = %d %q, want 400 invalid", r.method, r.path, name, code, resp)
			}
		}
	}
}

func TestWriteRoutesNeedAToken(t *testing.T) {
	t.Parallel()
	ts := writeServer(t, okStub())
	req, _ := http.NewRequest("POST", ts.URL+"/v1/issues", strings.NewReader(`{"title":"t"}`))
	req.Header.Set(api.SchemaHeader, "3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("POST /v1/issues without a token = %d, want 401", resp.StatusCode)
	}
}
