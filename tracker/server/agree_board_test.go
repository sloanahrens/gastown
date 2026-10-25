package server

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// Unit tier: the real client against the real routes with a stub store.
func TestClientBoardReadsAgreeWithTheRoutes(t *testing.T) {
	t.Parallel()
	ts := boardServer(t, boardStub{
		ready: func(_ context.Context, f api.ReadyFilter) (api.IssuePage, error) {
			switch f.Cursor {
			case "":
				return api.IssuePage{Issues: []api.Issue{{ID: "gt-1"}}, NextCursor: "p2"}, nil
			case "p2":
				return api.IssuePage{Issues: []api.Issue{{ID: "gt-2"}}}, nil
			}
			return api.IssuePage{}, api.Invalid("bad cursor %q", f.Cursor)
		},
		blocked: func(_ context.Context, f api.ReadyFilter) (api.IssuePage, error) {
			return api.IssuePage{Issues: []api.Issue{{ID: "gt-b", Assignee: f.Rig}}}, nil
		},
		children: func(_ context.Context, p []string) (map[string][]api.Issue, error) {
			out := map[string][]api.Issue{}
			for _, id := range p {
				if id == "gt-p" {
					out[id] = []api.Issue{{ID: "gt-p.1"}}
				}
			}
			return out, nil
		},
	})
	c, err := client.New(client.Config{BaseURL: ts.URL, Token: "good-token", SchemaLevel: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	all, err := c.ReadyAll(ctx, api.ReadyFilter{})
	if err != nil || len(all) != 2 || all[0].ID != "gt-1" || all[1].ID != "gt-2" {
		t.Errorf("ReadyAll = %v, %v; want both pages in order", all, err)
	}
	if _, err := c.Ready(ctx, api.ReadyFilter{Cursor: "junk"}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("Ready bad cursor = %v, want api.ErrInvalid", err)
	}
	if page, err := c.Blocked(ctx, api.ReadyFilter{Rig: "gt"}); err != nil || len(page.Issues) != 1 || page.Issues[0].Assignee != "gt" {
		t.Errorf("Blocked = %+v, %v; want the filter delivered", page, err)
	}
	kids, err := c.ChildrenOf(ctx, []string{"gt-p", "gt-none"})
	if err != nil || len(kids["gt-p"]) != 1 || len(kids) != 1 {
		t.Errorf("ChildrenOf = %v, %v; want only the parent that has children", kids, err)
	}
}
