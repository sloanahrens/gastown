package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// batchStub answers Batch; any other method panics on the nil embedded
// interface, which a test shows as a failure.
type batchStub struct {
	store.Store
	batch func(ctx context.Context, req api.BatchRequest) (api.BatchResult, error)
}

func (s batchStub) Batch(ctx context.Context, req api.BatchRequest) (api.BatchResult, error) {
	return s.batch(ctx, req)
}

// Unit tier: httptest, stub store, no Postgres; milliseconds per case.
func TestBatchPassesTheRequestAndAnswersOneResultPerOperation(t *testing.T) {
	t.Parallel()
	var got api.BatchRequest
	var ctxActor string
	titled := "new"
	ts := writeServer(t, batchStub{batch: func(ctx context.Context, req api.BatchRequest) (api.BatchResult, error) {
		got, ctxActor = req, api.ActorFrom(ctx)
		return api.BatchResult{}, nil // a nil slice from the store
	}})
	code, body := writeCall(t, "POST", ts.URL+"/v1/batch", `{"operations":[
		{"op":"create","create":{"rig":"gt","title":"t"}},
		{"op":"update","update":{"id":"gt-a","patch":{"title":"new"},"guard":{"if_status":"open"}}},
		{"op":"comment","comment":{"id":"gt-a","text":"hi"}},
		{"op":"close","close":{"ids":["gt-a"],"reason":"done"}}]}`)
	if code != http.StatusOK || !strings.Contains(body, `"results":[]`) {
		t.Fatalf("batch = %d %q, want 200 and an empty results list, not null", code, body)
	}
	if ctxActor != tokenActor {
		t.Errorf("actor on the context = %q, want the token's %q", ctxActor, tokenActor)
	}
	if len(got.Operations) != 4 || got.Operations[0].Create == nil || got.Operations[0].Create.Title != "t" ||
		got.Operations[1].Update == nil || got.Operations[1].Update.ID != "gt-a" || got.Operations[1].Update.Patch.Title == nil || *got.Operations[1].Update.Patch.Title != titled ||
		got.Operations[1].Update.Guard.IfStatus == nil || *got.Operations[1].Update.Guard.IfStatus != "open" ||
		got.Operations[2].Comment == nil || got.Operations[3].Close == nil || got.Operations[3].Close.Reason != "done" {
		t.Errorf("the store got %+v, want every operation passed through", got)
	}
}

func TestBatchAnswersTheResultsItGets(t *testing.T) {
	t.Parallel()
	made := api.Issue{ID: "gt-new", Title: "t"}
	ts := writeServer(t, batchStub{batch: func(context.Context, api.BatchRequest) (api.BatchResult, error) {
		return api.BatchResult{Results: []api.BatchOperationResult{
			{Index: 0, IDs: []string{"gt-new"}, Issue: &made},
			{Index: 1, IDs: []string{"gt-a"}},
		}}, nil
	}})
	code, body := writeCall(t, "POST", ts.URL+"/v1/batch", `{"operations":[{"op":"create","create":{"rig":"gt","title":"t"}},{"op":"update","update":{"id":"gt-a","patch":{}}}]}`)
	if code != http.StatusOK {
		t.Fatalf("batch = %d %q, want 200", code, body)
	}
	for _, want := range []string{`"index":0`, `"ids":["gt-new"]`, `"id":"gt-new"`, `"index":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %s", body, want)
		}
	}
}

// A failing store error reaches the client with its kind and, for a batch, the
// index of the operation the store refused.
func TestBatchMapsStoreErrorsAndNamesTheFailingIndex(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   api.Kind
		leak   string
	}{
		"invalid":   {api.BatchError(1, api.Invalid("no title")), 400, api.KindInvalid, ""},
		"not found": {api.BatchError(2, api.NotFound("gt-x")), 404, api.KindNotFound, ""},
		"conflict":  {api.BatchError(0, api.Conflict("taken")), 409, api.KindConflict, ""},
		"internal":  {context.DeadlineExceeded, 503, api.KindUnavailable, "deadline"},
	} {
		ts := writeServer(t, batchStub{batch: func(context.Context, api.BatchRequest) (api.BatchResult, error) {
			return api.BatchResult{}, tc.err
		}})
		code, body := writeCall(t, "POST", ts.URL+"/v1/batch", `{"operations":[{"op":"create","create":{"rig":"gt","title":"t"}}]}`)
		if e := decodeErr(t, body); code != tc.status || e.Kind != tc.kind {
			t.Errorf("%s: %d %q, want %d %s", name, code, body, tc.status, tc.kind)
		}
		if tc.leak != "" && strings.Contains(body, tc.leak) {
			t.Errorf("%s: the response leaks the cause: %q", name, body)
		}
		if tc.kind != api.KindUnavailable && !strings.Contains(body, "operation ") {
			t.Errorf("%s: body %q does not name the failing operation", name, body)
		}
	}
}

func TestBatchRefusesEmptyAndUnknownFieldBodies(t *testing.T) {
	t.Parallel()
	ts := writeServer(t, batchStub{})
	for name, body := range map[string]string{
		"empty":                   "",
		"unknown field":           `{"operations":[],"surprise":1}`,
		"unknown operation field": `{"operations":[{"op":"create","create":{"rig":"gt","title":"t"},"surprise":1}]}`,
		"not json":                `{`,
		"trailing data":           `{}{}`,
	} {
		if code, resp := writeCall(t, "POST", ts.URL+"/v1/batch", body); code != 400 {
			t.Errorf("%s = %d %q, want 400", name, code, resp)
		}
	}
}

func TestBatchNeedsAToken(t *testing.T) {
	t.Parallel()
	ts := writeServer(t, batchStub{})
	req, err := http.NewRequest("POST", ts.URL+"/v1/batch", strings.NewReader(`{"operations":[{"op":"create","create":{"rig":"gt","title":"t"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.SchemaHeader, "3")
	resp, err := writeHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("batch without a token = %d, want 401", resp.StatusCode)
	}
}

// A batch of no operations is the store's refusal to write, not a handler
// rule: the handler passes it through so the message names the bound once.
func TestAnEmptyBatchReachesTheStore(t *testing.T) {
	t.Parallel()
	calls := 0
	ts := writeServer(t, batchStub{batch: func(context.Context, api.BatchRequest) (api.BatchResult, error) {
		calls++
		return api.BatchResult{}, errors.New("store refused")
	}})
	if code, _ := writeCall(t, "POST", ts.URL+"/v1/batch", `{"operations":[]}`); code != http.StatusServiceUnavailable {
		t.Errorf("batch of no operations = %d, want the store's error answered", code)
	}
	if calls != 1 {
		t.Errorf("the store was called %d times, want 1: the handler does not decide what an empty batch means", calls)
	}
}
