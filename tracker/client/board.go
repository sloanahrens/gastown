package client

import (
	"context"
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// Ready returns one page of open, unblocked, dispatchable issues matching f;
// pass the page's NextCursor in f.Cursor for the next one.
func (c *Client) Ready(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error) {
	var out api.IssuePage
	err := c.do(ctx, http.MethodPost, "/v1/ready", f, &out, true)
	return out, err
}

// ReadyAll returns every ready issue matching f, following the cursor to the end.
func (c *Client) ReadyAll(ctx context.Context, f api.ReadyFilter) ([]api.Issue, error) {
	return All(ctx, func(ctx context.Context, cursor string) ([]api.Issue, string, error) {
		f.Cursor = cursor
		page, err := c.Ready(ctx, f)
		return page.Issues, page.NextCursor, err
	})
}

// Blocked returns one page of issues held by an open blocker, matching f.
func (c *Client) Blocked(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error) {
	var out api.IssuePage
	err := c.do(ctx, http.MethodPost, "/v1/blocked", f, &out, true)
	return out, err
}

// ChildrenOf returns the direct children of each of parents, keyed by parent;
// a parent with no children is absent. The map is never nil.
func (c *Client) ChildrenOf(ctx context.Context, parents []string) (map[string][]api.Issue, error) {
	var out api.ChildrenOfResponse
	if err := c.do(ctx, http.MethodPost, "/v1/issues:children-of", api.ChildrenOfRequest{Parents: parents}, &out, true); err != nil {
		return nil, err
	}
	if out.Children == nil {
		out.Children = map[string][]api.Issue{}
	}
	return out.Children, nil
}

// DispatchBoard returns the dispatch tick's reads in one snapshot: per rig the
// ready issues, the held issues and the children of the ready ones, plus the
// probed statuses. Its maps and slices are never nil.
func (c *Client) DispatchBoard(ctx context.Context, req api.DispatchBoardRequest) (api.DispatchBoardResponse, error) {
	var out api.DispatchBoardResponse
	if err := c.do(ctx, http.MethodPost, "/v1/board/dispatch", req, &out, true); err != nil {
		return api.DispatchBoardResponse{}, err
	}
	if out.Rigs == nil {
		out.Rigs = map[string]api.RigBoard{}
	}
	if out.Probes == nil {
		out.Probes = map[string]api.Probe{}
	}
	if out.Errors == nil {
		out.Errors = map[string]string{}
	}
	for rig, b := range out.Rigs {
		if b.Ready == nil {
			b.Ready = []api.Issue{}
		}
		if b.Held == nil {
			b.Held = []api.HeldBead{}
		}
		if b.Children == nil {
			b.Children = map[string][]api.ChildStatus{}
		}
		out.Rigs[rig] = b
	}
	return out, nil
}
