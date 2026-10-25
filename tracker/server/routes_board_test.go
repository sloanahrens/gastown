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

// boardStub answers the three board reads; any other method panics on the nil
// embedded interface, which a test shows as a failure.
type boardStub struct {
	store.Store
	ready    func(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error)
	blocked  func(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error)
	children func(ctx context.Context, parents []string) (map[string][]api.Issue, error)
}

func (s boardStub) Ready(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error) {
	return s.ready(ctx, f)
}

func (s boardStub) Blocked(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error) {
	return s.blocked(ctx, f)
}

func (s boardStub) ChildrenOf(ctx context.Context, p []string) (map[string][]api.Issue, error) {
	return s.children(ctx, p)
}

func boardServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	s := New(Config{Auth: fakeAuth{"good-token": "town/crew/dipper"}, Health: fakeHealth{}, SchemaLevel: 3, Store: st})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func boardPost(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
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

func emptyPage(context.Context, api.ReadyFilter) (api.IssuePage, error) { return api.IssuePage{}, nil }

// Unit tier: httptest, stub store, no Postgres; milliseconds per case.
func TestReadyAndBlockedPassTheFilterAndAnswerEmptyAsAnArray(t *testing.T) {
	t.Parallel()
	var gotReady, gotBlocked api.ReadyFilter
	ts := boardServer(t, boardStub{
		ready: func(_ context.Context, f api.ReadyFilter) (api.IssuePage, error) {
			gotReady = f
			return api.IssuePage{}, nil
		},
		blocked: func(_ context.Context, f api.ReadyFilter) (api.IssuePage, error) {
			gotBlocked = f
			return api.IssuePage{}, nil
		},
	})
	body := `{"rig":"gt","parent":"gt-p","molecule":"gt-m","unassigned":true,"exclude_labels":["x"],"exclude_types":["agent"],"gated":true,"limit":9,"cursor":"c"}`
	for path, got := range map[string]*api.ReadyFilter{"/v1/ready": &gotReady, "/v1/blocked": &gotBlocked} {
		code, resp := boardPost(t, ts.URL+path, body)
		if code != 200 || !strings.Contains(resp, `"issues":[]`) {
			t.Errorf("%s = %d %q, want 200 and an empty array, not null", path, code, resp)
		}
		if got.Rig != "gt" || got.Parent != "gt-p" || got.Molecule != "gt-m" || !got.Unassigned || len(got.ExcludeLabels) != 1 ||
			len(got.ExcludeTypes) != 1 || got.ExcludeTypes[0] != "agent" || !got.Gated || got.Limit != 9 || got.Cursor != "c" {
			t.Errorf("%s: the store got %+v, want every field passed through", path, *got)
		}
	}
	if gotReady.Rig == "" || gotBlocked.Rig == "" {
		t.Error("each route must call its own store method")
	}
}

func TestReadyAndBlockedAreNotMixedUp(t *testing.T) {
	t.Parallel()
	ts := boardServer(t, boardStub{
		ready: func(context.Context, api.ReadyFilter) (api.IssuePage, error) {
			return api.IssuePage{Issues: []api.Issue{{ID: "gt-ready"}}}, nil
		},
		blocked: func(context.Context, api.ReadyFilter) (api.IssuePage, error) {
			return api.IssuePage{Issues: []api.Issue{{ID: "gt-blocked"}}}, nil
		},
	})
	if _, body := boardPost(t, ts.URL+"/v1/ready", `{}`); !strings.Contains(body, "gt-ready") {
		t.Errorf("/v1/ready = %q, want the ready issue", body)
	}
	if _, body := boardPost(t, ts.URL+"/v1/blocked", `{}`); !strings.Contains(body, "gt-blocked") {
		t.Errorf("/v1/blocked = %q, want the blocked issue", body)
	}
}

func TestChildrenOfMapsParentsAndNeverReturnsNull(t *testing.T) {
	t.Parallel()
	var asked []string
	ts := boardServer(t, boardStub{children: func(_ context.Context, p []string) (map[string][]api.Issue, error) {
		asked = p
		return nil, nil
	}})
	code, body := boardPost(t, ts.URL+"/v1/issues:children-of", `{"parents":["gt-p","gt-q"]}`)
	if code != 200 || !strings.Contains(body, `"children":{}`) {
		t.Errorf("children-of = %d %q, want 200 and an empty object, not null", code, body)
	}
	if len(asked) != 2 || asked[0] != "gt-p" || asked[1] != "gt-q" {
		t.Errorf("the store was asked for %v, want both parents in order", asked)
	}
}

func TestBoardRoutesPassAPIErrorsAndHideInternalOnes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   api.Kind
		leak   string
	}{
		"invalid":  {api.Invalid("bad cursor"), 400, api.KindInvalid, ""},
		"internal": {context.DeadlineExceeded, 503, api.KindUnavailable, "deadline"},
	} {
		ts := boardServer(t, boardStub{
			ready:    func(context.Context, api.ReadyFilter) (api.IssuePage, error) { return api.IssuePage{}, tc.err },
			blocked:  func(context.Context, api.ReadyFilter) (api.IssuePage, error) { return api.IssuePage{}, tc.err },
			children: func(context.Context, []string) (map[string][]api.Issue, error) { return nil, tc.err },
		})
		for _, path := range []string{"/v1/ready", "/v1/blocked", "/v1/issues:children-of"} {
			code, body := boardPost(t, ts.URL+path, `{}`)
			if e := decodeErr(t, body); code != tc.status || e.Kind != tc.kind {
				t.Errorf("%s %s: %d %q, want %d %s", name, path, code, body, tc.status, tc.kind)
			}
			if tc.leak != "" && strings.Contains(body, tc.leak) {
				t.Errorf("%s %s: the response leaks the cause: %q", name, path, body)
			}
		}
	}
}

func TestBoardRoutesRefuseBadBodiesAsInvalid(t *testing.T) {
	t.Parallel()
	ts := boardServer(t, boardStub{})
	for _, path := range []string{"/v1/ready", "/v1/blocked", "/v1/issues:children-of"} {
		for name, body := range map[string]string{"empty": "", "unknown field": `{"surprise":1}`, "not json": `{`, "trailing data": `{}{}`} {
			code, resp := boardPost(t, ts.URL+path, body)
			if e := decodeErr(t, resp); code != 400 || e.Kind != api.KindInvalid {
				t.Errorf("%s %s = %d %q, want 400 invalid", path, name, code, resp)
			}
		}
	}
}

func TestBoardRoutesNeedAToken(t *testing.T) {
	t.Parallel()
	ts := boardServer(t, boardStub{ready: emptyPage})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/ready", strings.NewReader(`{}`))
	req.Header.Set(api.SchemaHeader, "3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("POST /v1/ready without a token = %d, want 401", resp.StatusCode)
	}
}
