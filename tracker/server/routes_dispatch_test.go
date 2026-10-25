package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/store"
)

// dispatchStub answers DispatchBoard; any other method panics on the nil
// embedded interface, which a test shows as a failure.
type dispatchStub struct {
	store.Store
	board func(ctx context.Context, req api.DispatchBoardRequest) (api.DispatchBoardResponse, error)
}

func (s dispatchStub) DispatchBoard(ctx context.Context, req api.DispatchBoardRequest) (api.DispatchBoardResponse, error) {
	return s.board(ctx, req)
}

// Unit tier: httptest, stub store, no Postgres; milliseconds per case.
func TestDispatchBoardPassesTheRequestAndNeverAnswersNull(t *testing.T) {
	t.Parallel()
	var got api.DispatchBoardRequest
	ts := boardServer(t, dispatchStub{board: func(_ context.Context, req api.DispatchBoardRequest) (api.DispatchBoardResponse, error) {
		got = req
		// A store answering with nil collections must still reach the wire as {} and [].
		return api.DispatchBoardResponse{Rigs: map[string]api.RigBoard{"gt": {}}}, nil
	}})
	code, body := boardPost(t, ts.URL+"/v1/board/dispatch",
		`{"rigs":["gt","hq"],"exclude_labels":["x"],"exclude_types":["epic"],"held_statuses":["hooked"],"probe_ids":["gt-r"]}`)
	if code != 200 {
		t.Fatalf("status = %d %q, want 200", code, body)
	}
	for _, want := range []string{`"ready":[]`, `"held":[]`, `"children":{}`, `"probes":{}`, `"errors":{}`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %s: collections must be empty, never null", body, want)
		}
	}
	if len(got.Rigs) != 2 || got.Rigs[1] != "hq" || got.ExcludeLabels[0] != "x" || got.ExcludeTypes[0] != "epic" || got.HeldStatuses[0] != "hooked" || got.ProbeIDs[0] != "gt-r" {
		t.Errorf("the store got %+v, want every field passed through", got)
	}
}

func TestDispatchBoardPassesAPIErrorsAndHidesInternalOnes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   api.Kind
		leak   string
	}{
		"invalid":  {api.Invalid("at least one rig is required"), 400, api.KindInvalid, ""},
		"internal": {context.DeadlineExceeded, 503, api.KindUnavailable, "deadline"},
	} {
		ts := boardServer(t, dispatchStub{board: func(context.Context, api.DispatchBoardRequest) (api.DispatchBoardResponse, error) {
			return api.DispatchBoardResponse{}, tc.err
		}})
		code, body := boardPost(t, ts.URL+"/v1/board/dispatch", `{"rigs":["gt"]}`)
		if e := decodeErr(t, body); code != tc.status || e.Kind != tc.kind {
			t.Errorf("%s: %d %q, want %d %s", name, code, body, tc.status, tc.kind)
		}
		if tc.leak != "" && strings.Contains(body, tc.leak) {
			t.Errorf("%s: the response leaks the cause: %q", name, body)
		}
	}
}

func TestDispatchBoardRefusesBadBodiesAsInvalid(t *testing.T) {
	t.Parallel()
	ts := boardServer(t, dispatchStub{})
	for name, body := range map[string]string{"empty": "", "unknown field": `{"surprise":1}`, "not json": `{`, "trailing data": `{}{}`} {
		code, resp := boardPost(t, ts.URL+"/v1/board/dispatch", body)
		if e := decodeErr(t, resp); code != 400 || e.Kind != api.KindInvalid {
			t.Errorf("%s = %d %q, want 400 invalid", name, code, resp)
		}
	}
}

func TestClientDispatchBoardAgreesWithTheRoute(t *testing.T) {
	t.Parallel()
	ts := boardServer(t, dispatchStub{board: func(_ context.Context, req api.DispatchBoardRequest) (api.DispatchBoardResponse, error) {
		if len(req.Rigs) == 0 {
			return api.DispatchBoardResponse{}, api.Invalid("at least one rig is required")
		}
		return api.DispatchBoardResponse{
			Rigs: map[string]api.RigBoard{"gt": {
				Ready:    []api.Issue{{ID: "gt-1", Title: "one", Description: "body"}},
				Held:     []api.HeldBead{{ID: "gt-2", Type: "task", Assignee: "p", Description: "d"}},
				Children: map[string][]api.ChildStatus{"gt-1": {{ID: "gt-1.1", Status: "open"}}},
			}},
			Probes: map[string]api.Probe{"gt-r": {Found: true, Status: "open"}},
			Errors: map[string]string{"nope": "unknown rig"},
		}, nil
	}})
	c, err := client.New(client.Config{BaseURL: ts.URL, Token: "good-token", SchemaLevel: 3})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.DispatchBoard(context.Background(), api.DispatchBoardRequest{Rigs: []string{"gt", "nope"}})
	if err != nil {
		t.Fatal(err)
	}
	gt := got.Rigs["gt"]
	if len(gt.Ready) != 1 || gt.Ready[0].Description != "body" || len(gt.Held) != 1 || gt.Held[0].Assignee != "p" ||
		len(gt.Children["gt-1"]) != 1 || gt.Children["gt-1"][0].Status != "open" ||
		!got.Probes["gt-r"].Found || got.Errors["nope"] != "unknown rig" {
		t.Errorf("DispatchBoard = %+v, want every part of the board to cross the wire", got)
	}
	if _, err := c.DispatchBoard(context.Background(), api.DispatchBoardRequest{}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("DispatchBoard with no rig = %v, want api.ErrInvalid", err)
	}
}
