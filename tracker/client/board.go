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
