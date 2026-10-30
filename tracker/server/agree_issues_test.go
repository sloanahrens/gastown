package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

func agreeClient(t *testing.T, url string) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{BaseURL: url, Token: "good-token", SchemaLevel: 3})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Unit tier: the real client against the real routes with a stub store.
func TestClientReadsAgreeWithTheRoutes(t *testing.T) {
	t.Parallel()
	all := map[string]api.Issue{"gt-a": {ID: "gt-a", Title: "a"}, "gt-b": {ID: "gt-b", Title: "b"}}
	ts := stubServer(t, issueStub{
		get: func(_ context.Context, ids []string) (map[string]api.Issue, error) {
			out := map[string]api.Issue{}
			for _, id := range ids {
				if is, ok := all[id]; ok {
					out[id] = is
				}
			}
			return out, nil
		},
		list: func(_ context.Context, f api.ListFilter) (api.IssuePage, error) {
			// Two pages: cursor "" gives gt-a, cursor "p2" gives gt-b.
			switch f.Cursor {
			case "":
				return api.IssuePage{Issues: []api.Issue{all["gt-a"]}, NextCursor: "p2"}, nil
			case "p2":
				return api.IssuePage{Issues: []api.Issue{all["gt-b"]}}, nil
			}
			return api.IssuePage{}, api.Invalid("bad cursor %q", f.Cursor)
		},
	})
	c := agreeClient(t, ts.URL)
	ctx := context.Background()
	if is, err := c.Show(ctx, "gt-a"); err != nil || is.Title != "a" {
		t.Errorf("Show = %+v, %v", is, err)
	}
	if _, err := c.Show(ctx, "gt-zzz"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Show missing = %v, want api.ErrNotFound", err)
	}
	m, err := c.ShowMany(ctx, []string{"gt-a", "gt-zzz", "gt-b"})
	if err != nil || len(m) != 2 || m["gt-b"].Title != "b" {
		t.Errorf("ShowMany = %v, %v; want the two that exist", m, err)
	}
	if m, err := c.ShowMany(ctx, []string{"gt-zzz"}); err != nil || m == nil || len(m) != 0 {
		t.Errorf("ShowMany none found = %v, %v; want an empty non-nil map", m, err)
	}
	got, err := c.ListAll(ctx, api.ListFilter{})
	if err != nil || len(got) != 2 || fmt.Sprintf("%s %s", got[0].ID, got[1].ID) != "gt-a gt-b" {
		t.Errorf("ListAll = %v, %v; want both pages in order", got, err)
	}
	if _, err := c.List(ctx, api.ListFilter{Cursor: "junk"}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("List bad cursor = %v, want api.ErrInvalid", err)
	}
}

// TestClientCountAgreesWithTheListItCounts pins the pair the wisp reaper sizes
// its work with: over one store, Count answers for every row List walks.
func TestClientCountAgreesWithTheListItCounts(t *testing.T) {
	t.Parallel()
	wisps := []api.Issue{
		{ID: "gt-wisp-a", Title: "a", Ephemeral: true},
		{ID: "gt-wisp-b", Title: "b", Ephemeral: true},
		{ID: "gt-wisp-c", Title: "c", Ephemeral: true},
	}
	ts := stubServer(t, issueStub{
		list: func(_ context.Context, f api.ListFilter) (api.IssuePage, error) {
			return api.IssuePage{Issues: wisps}, nil
		},
		count: func(_ context.Context, f api.ListFilter) (int, error) { return len(wisps), nil },
	})
	c := agreeClient(t, ts.URL)
	f := api.ListFilter{Ephemeral: true, Statuses: []string{"closed"}}
	got, err := c.ListAll(t.Context(), f)
	if err != nil || len(got) != 3 {
		t.Fatalf("ListAll = %v, %v; want the three wisps", got, err)
	}
	n, err := c.Count(t.Context(), f)
	if err != nil || n != len(got) {
		t.Errorf("Count = %d, %v; want %d, the length of the list it counts", n, err, len(got))
	}
}
