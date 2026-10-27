package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/store"
)

// snapshotStub answers TownSnapshot; any other method panics on the nil
// embedded interface, which a test shows as a failure.
type snapshotStub struct {
	store.Store
	snapshot func(ctx context.Context, req api.SnapshotRequest) (api.TownSnapshot, error)
}

func (s snapshotStub) TownSnapshot(ctx context.Context, req api.SnapshotRequest) (api.TownSnapshot, error) {
	return s.snapshot(ctx, req)
}

// Unit tier: httptest, stub store, no Postgres; milliseconds per case.
func TestClientSnapshotAgreesWithTheRoute(t *testing.T) {
	t.Parallel()
	var got api.SnapshotRequest
	ts := stubServer(t, snapshotStub{snapshot: func(_ context.Context, req api.SnapshotRequest) (api.TownSnapshot, error) {
		got = req
		return api.TownSnapshot{
			Issues:     []api.SnapshotIssue{{Issue: api.Issue{ID: "gt-1", Title: "one"}, Blocked: true}},
			Counts:     api.SnapshotCounts{Total: 3, ByStatus: map[string]int{"open": 3}, ByRig: map[string]int{"gt": 3}, Blocked: 1, BlockedRigs: map[string]int{"gt": 1}},
			Truncated:  true,
			NextCursor: "p2",
		}, nil
	}})
	c, err := client.New(client.Config{BaseURL: ts.URL, Token: "good-token", SchemaLevel: 3})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(context.Background(), api.SnapshotRequest{Limit: 2, Cursor: "p1"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got.Limit != 2 || got.Cursor != "p1" {
		t.Errorf("the store got %+v, want the page request passed through", got)
	}
	if len(snap.Issues) != 1 || snap.Issues[0].ID != "gt-1" || !snap.Issues[0].Blocked {
		t.Errorf("issues = %+v, want the row with its blocked verdict", snap.Issues)
	}
	if !snap.Truncated || snap.NextCursor != "p2" {
		t.Errorf("truncated/cursor = %v/%q, want the cut page passed through", snap.Truncated, snap.NextCursor)
	}
	if snap.Counts.Total != 3 || snap.Counts.Blocked != 1 || snap.Counts.BlockedRigs["gt"] != 1 {
		t.Errorf("counts = %+v, want the store's counts passed through", snap.Counts)
	}
}

func TestSnapshotAnswersEmptyCollectionsAndRefusesABadLimit(t *testing.T) {
	t.Parallel()
	ts := stubServer(t, snapshotStub{snapshot: func(context.Context, api.SnapshotRequest) (api.TownSnapshot, error) {
		return api.TownSnapshot{}, nil
	}})
	code, body := call(t, http.MethodGet, ts.URL+"/v1/snapshot/town", "")
	if code != 200 {
		t.Fatalf("status = %d %q, want 200", code, body)
	}
	for _, want := range []string{`"issues":[]`, `"by_status":{}`, `"by_rig":{}`, `"blocked_rigs":{}`, `"truncated":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %s: collections must be empty, never null", body, want)
		}
	}
	if strings.Contains(body, "next_cursor") {
		t.Errorf("body %q carries a cursor on a page that is not cut", body)
	}
	code, body = call(t, http.MethodGet, ts.URL+"/v1/snapshot/town?limit=x", "")
	if e := decodeErr(t, body); code != 400 || e.Kind != api.KindInvalid {
		t.Errorf("bad limit = %d %q, want 400 invalid", code, body)
	}
}
