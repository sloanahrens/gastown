package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/steveyegge/gastown/tracker/api"
)

// Show returns one issue, or an error satisfying errors.Is(err, api.ErrNotFound).
func (c *Client) Show(ctx context.Context, id string) (api.Issue, error) {
	var out api.Issue
	err := c.do(ctx, http.MethodGet, "/v1/issues/"+url.PathEscape(id), nil, &out, true)
	return out, err
}

// ShowMany returns the issues that exist among ids, keyed by id; a missing id
// is absent from the map. The map is never nil.
func (c *Client) ShowMany(ctx context.Context, ids []string) (map[string]api.Issue, error) {
	var out api.ShowManyResponse
	if err := c.do(ctx, http.MethodPost, "/v1/issues:show-many", api.ShowManyRequest{IDs: ids}, &out, true); err != nil {
		return nil, err
	}
	if out.Issues == nil {
		out.Issues = map[string]api.Issue{}
	}
	return out.Issues, nil
}

// List returns one page of issues matching f; pass the page's NextCursor in
// f.Cursor for the next one.
func (c *Client) List(ctx context.Context, f api.ListFilter) (api.IssuePage, error) {
	var out api.IssuePage
	err := c.do(ctx, http.MethodPost, "/v1/issues:list", f, &out, true)
	return out, err
}

// Count returns how many issues f matches in all. It sends f as given, so a
// filter that carries a limit or a cursor comes back as an api.ErrInvalid from
// the server rather than as a count of one page.
func (c *Client) Count(ctx context.Context, f api.ListFilter) (int, error) {
	var out api.CountResponse
	if err := c.do(ctx, http.MethodPost, "/v1/issues:count", api.CountRequest{Filter: f}, &out, true); err != nil {
		return 0, err
	}
	return out.Count, nil
}

// Assigned returns one page of the issues assigned to req.Assignees in one of
// req.Statuses, keyed by assignee; pass the page's NextCursor in req.Cursor
// for the next one. The map is never nil.
func (c *Client) Assigned(ctx context.Context, req api.AssignedRequest) (api.AssignedPage, error) {
	var out api.AssignedPage
	if err := c.do(ctx, http.MethodPost, "/v1/assigned", req, &out, true); err != nil {
		return api.AssignedPage{}, err
	}
	if out.Assigned == nil {
		out.Assigned = map[string][]api.Issue{}
	}
	return out, nil
}

// ListAll returns every issue matching f, following the cursor to the end.
func (c *Client) ListAll(ctx context.Context, f api.ListFilter) ([]api.Issue, error) {
	return All(ctx, func(ctx context.Context, cursor string) ([]api.Issue, string, error) {
		f.Cursor = cursor
		page, err := c.List(ctx, f)
		return page.Issues, page.NextCursor, err
	})
}
