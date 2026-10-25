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

// linkStub answers the comment and dependency methods; any other method
// panics on the nil embedded interface, which a test shows as a failure.
type linkStub struct {
	store.Store
	addComment func(ctx context.Context, id, author, text string) error
	comments   func(ctx context.Context, id string) ([]api.Comment, error)
	addDep     func(ctx context.Context, issue, on, typ string) error
	removeDep  func(ctx context.Context, issue, on string) error
	deps       func(ctx context.Context, id, typ string) ([]api.IssueDep, error)
}

func (s linkStub) AddComment(ctx context.Context, id, a, t string) error {
	return s.addComment(ctx, id, a, t)
}
func (s linkStub) Comments(ctx context.Context, id string) ([]api.Comment, error) {
	return s.comments(ctx, id)
}
func (s linkStub) AddDependency(ctx context.Context, i, o, t string) error {
	return s.addDep(ctx, i, o, t)
}
func (s linkStub) RemoveDependency(ctx context.Context, i, o string) error {
	return s.removeDep(ctx, i, o)
}
func (s linkStub) Dependencies(ctx context.Context, id, t string) ([]api.IssueDep, error) {
	return s.deps(ctx, id, t)
}

func linkServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	s := New(Config{Auth: fakeAuth{"good-token": "town/crew/dipper"}, Health: fakeHealth{}, SchemaLevel: 3, Store: st})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func linkCall(t *testing.T, method, url, body string) (int, string) {
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

const linkTokenActor = "town/crew/dipper"

func okLinks() linkStub {
	return linkStub{
		addComment: func(context.Context, string, string, string) error { return nil },
		comments:   func(context.Context, string) ([]api.Comment, error) { return nil, nil },
		addDep:     func(context.Context, string, string, string) error { return nil },
		removeDep:  func(context.Context, string, string) error { return nil },
		deps:       func(context.Context, string, string) ([]api.IssueDep, error) { return nil, nil },
	}
}

// Unit tier: httptest, stub store, no Postgres; milliseconds per case.
func TestAddCommentDefaultsTheAuthorToTheTokensActorAndHonorsAnExplicitOne(t *testing.T) {
	t.Parallel()
	var id, author, text string
	st := okLinks()
	st.addComment = func(_ context.Context, i, a, tx string) error { id, author, text = i, a, tx; return nil }
	ts := linkServer(t, st)
	if code, body := linkCall(t, "POST", ts.URL+"/v1/issues/gt-a/comments", `{"text":"hello"}`); code != 204 || body != "" {
		t.Fatalf("add comment = %d %q, want 204", code, body)
	}
	if id != "gt-a" || author != linkTokenActor || text != "hello" {
		t.Errorf("got id=%q author=%q text=%q, want the token's actor as the default author", id, author, text)
	}
	if code, _ := linkCall(t, "POST", ts.URL+"/v1/issues/gt-a/comments", `{"text":"x","author":"town/witness"}`); code != 204 || author != "town/witness" {
		t.Errorf("explicit author: %d, store got %q, want 204 and town/witness", code, author)
	}
}

func TestListCommentsAnswersEmptyAsAnArrayAndMissingAs404(t *testing.T) {
	t.Parallel()
	st := okLinks()
	st.comments = func(_ context.Context, id string) ([]api.Comment, error) {
		if id == "gt-none" {
			return nil, api.NotFound(id)
		}
		return nil, nil
	}
	ts := linkServer(t, st)
	if code, body := linkCall(t, "GET", ts.URL+"/v1/issues/gt-a/comments", ""); code != 200 || !strings.Contains(body, `"comments":[]`) {
		t.Errorf("empty comments = %d %q, want 200 and an empty array, not null", code, body)
	}
	code, body := linkCall(t, "GET", ts.URL+"/v1/issues/gt-none/comments", "")
	if e := decodeErr(t, body); code != 404 || e.Kind != api.KindNotFound {
		t.Errorf("missing issue = %d %q, want 404 not_found", code, body)
	}
}

func TestAddDependencyPassesTheBodyAndAnswers204(t *testing.T) {
	t.Parallel()
	var issue, on, typ string
	st := okLinks()
	st.addDep = func(_ context.Context, i, o, ty string) error { issue, on, typ = i, o, ty; return nil }
	ts := linkServer(t, st)
	code, body := linkCall(t, "POST", ts.URL+"/v1/issues/gt-a/dependencies", `{"depends_on":"external:om:om-7","type":"blocks"}`)
	if code != 204 || body != "" || issue != "gt-a" || on != "external:om:om-7" || typ != "blocks" {
		t.Errorf("add dependency = %d %q; store got %q %q %q", code, body, issue, on, typ)
	}
}

func TestRemoveDependencyMatchesAnExternalTargetAndMapsNotFound(t *testing.T) {
	t.Parallel()
	var issue, on string
	st := okLinks()
	st.removeDep = func(_ context.Context, i, o string) error {
		issue, on = i, o
		if o == "gt-gone" {
			return api.NotFound("dependency")
		}
		return nil
	}
	ts := linkServer(t, st)
	if code, body := linkCall(t, "DELETE", ts.URL+"/v1/issues/gt-a/dependencies/external:om:om-7", ""); code != 204 || body != "" || issue != "gt-a" || on != "external:om:om-7" {
		t.Errorf("remove external = %d %q; store got %q %q, want 204 and the full external reference", code, body, issue, on)
	}
	code, body := linkCall(t, "DELETE", ts.URL+"/v1/issues/gt-a/dependencies/gt-gone", "")
	if e := decodeErr(t, body); code != 404 || e.Kind != api.KindNotFound {
		t.Errorf("missing link = %d %q, want 404 not_found", code, body)
	}
}

func TestListDependenciesPassesTheTypeAndAnswersEmptyAsAnArray(t *testing.T) {
	t.Parallel()
	var gotType []string
	st := okLinks()
	st.deps = func(_ context.Context, _ string, typ string) ([]api.IssueDep, error) {
		gotType = append(gotType, typ)
		return nil, nil
	}
	ts := linkServer(t, st)
	for _, path := range []string{"/v1/issues/gt-a/dependencies", "/v1/issues/gt-a/dependencies?type=tracks"} {
		if code, body := linkCall(t, "GET", ts.URL+path, ""); code != 200 || !strings.Contains(body, `"dependencies":[]`) {
			t.Errorf("GET %s = %d %q, want 200 and an empty array, not null", path, code, body)
		}
	}
	if len(gotType) != 2 || gotType[0] != "" || gotType[1] != "tracks" {
		t.Errorf("types reaching the store = %q, want an empty string when absent, then tracks", gotType)
	}
}

func TestLinkRoutesMapStoreErrorsAndHideInternalOnes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   api.Kind
		leak   string
	}{
		"not found": {api.NotFound("gt-a"), 404, api.KindNotFound, ""},
		"conflict":  {api.Conflict("cycle"), 409, api.KindConflict, ""},
		"invalid":   {api.Invalid("bad type"), 400, api.KindInvalid, ""},
		"internal":  {context.DeadlineExceeded, 503, api.KindUnavailable, "deadline"},
	} {
		fail := tc.err
		st := linkStub{
			addComment: func(context.Context, string, string, string) error { return fail },
			comments:   func(context.Context, string) ([]api.Comment, error) { return nil, fail },
			addDep:     func(context.Context, string, string, string) error { return fail },
			removeDep:  func(context.Context, string, string) error { return fail },
			deps:       func(context.Context, string, string) ([]api.IssueDep, error) { return nil, fail },
		}
		ts := linkServer(t, st)
		for _, r := range []struct{ method, path, body string }{
			{"POST", "/v1/issues/gt-a/comments", `{"text":"x"}`}, {"GET", "/v1/issues/gt-a/comments", ""},
			{"POST", "/v1/issues/gt-a/dependencies", `{"depends_on":"gt-b","type":"blocks"}`},
			{"DELETE", "/v1/issues/gt-a/dependencies/gt-b", ""}, {"GET", "/v1/issues/gt-a/dependencies", ""},
		} {
			code, body := linkCall(t, r.method, ts.URL+r.path, r.body)
			if e := decodeErr(t, body); code != tc.status || e.Kind != tc.kind {
				t.Errorf("%s %s %s: %d %q, want %d %s", name, r.method, r.path, code, body, tc.status, tc.kind)
			}
			if tc.leak != "" && strings.Contains(body, tc.leak) {
				t.Errorf("%s %s: the response leaks the cause: %q", name, r.path, body)
			}
		}
	}
}

func TestLinkPostRoutesRefuseBadBodies(t *testing.T) {
	t.Parallel()
	ts := linkServer(t, linkStub{})
	for _, path := range []string{"/v1/issues/gt-a/comments", "/v1/issues/gt-a/dependencies"} {
		for name, body := range map[string]string{"empty": "", "unknown field": `{"surprise":1}`, "not json": `{`, "trailing data": `{}{}`} {
			code, resp := linkCall(t, "POST", ts.URL+path, body)
			if e := decodeErr(t, resp); code != 400 || e.Kind != api.KindInvalid {
				t.Errorf("POST %s %s = %d %q, want 400 invalid", path, name, code, resp)
			}
		}
	}
}
