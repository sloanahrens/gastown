package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/steveyegge/gastown/tracker/api"
)

// The write methods get exactly one attempt: a write whose answer was lost may
// or may not have been applied, so retrying it could apply it twice.

// Create creates an issue as the client's actor. A p.Actor is ignored by the
// server, which uses the token's.
func (c *Client) Create(ctx context.Context, p api.CreateParams) (api.Issue, error) {
	var out api.Issue
	err := c.do(ctx, http.MethodPost, "/v1/issues", p, &out, false)
	return out, err
}

// Update applies patch to the issue while guard holds; a guard that does not
// hold is an error satisfying errors.Is(err, api.ErrGuardFailed).
func (c *Client) Update(ctx context.Context, id string, patch api.UpdatePatch, guard api.Guard) error {
	return c.do(ctx, http.MethodPatch, "/v1/issues/"+url.PathEscape(id), api.UpdateRequest{Patch: patch, Guard: guard}, nil, false)
}

// Claim takes the issue for the client's actor in one step. Another actor
// holding it is an error satisfying errors.Is(err, api.ErrGuardFailed), so a
// caller that lost the race learns it without a read.
func (c *Client) Claim(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/issues/"+url.PathEscape(id)+"/claim", nil, nil, false)
}

// Release returns the client's actor's issue to open with no assignee. Not
// holding it is an error satisfying errors.Is(err, api.ErrGuardFailed).
func (c *Client) Release(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/issues/"+url.PathEscape(id)+"/release", nil, nil, false)
}

// Transfer hands the client's actor's issue to assignee, at status (the status
// the issue has when empty). Not holding it is an error satisfying
// errors.Is(err, api.ErrGuardFailed).
func (c *Client) Transfer(ctx context.Context, id, status, assignee string) error {
	return c.do(ctx, http.MethodPost, "/v1/issues/"+url.PathEscape(id)+"/transfer", api.TransferRequest{Status: status, Assignee: assignee}, nil, false)
}

// Close closes all of ids with reason, or none; force closes past open
// children and live blockers.
func (c *Client) Close(ctx context.Context, ids []string, reason string, force bool) error {
	return c.do(ctx, http.MethodPost, "/v1/issues:close", api.CloseRequest{IDs: ids, Reason: reason, Force: force}, nil, false)
}

// Delete deletes all of ids with their labels, comments and links, or none.
func (c *Client) Delete(ctx context.Context, ids []string) error {
	return c.do(ctx, http.MethodPost, "/v1/issues:delete", api.DeleteRequest{IDs: ids}, nil, false)
}

// CreateGraph creates the nodes and links of req in one transaction, or
// nothing, and returns the id created for each node key. Like every write it
// gets one attempt.
func (c *Client) CreateGraph(ctx context.Context, req api.GraphRequest) (api.GraphResult, error) {
	var out api.GraphResult
	if err := c.do(ctx, http.MethodPost, "/v1/graphs", req, &out, false); err != nil {
		return api.GraphResult{}, err
	}
	if out.IDs == nil {
		out.IDs = map[string]string{}
	}
	return out, nil
}
