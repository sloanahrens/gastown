package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/steveyegge/gastown/tracker/api"
)

// AddComment appends a comment to the issue. author is the comment's author;
// empty means the client's actor.
func (c *Client) AddComment(ctx context.Context, id, author, text string) error {
	return c.do(ctx, http.MethodPost, issuePath(id, "comments"), api.CommentRequest{Text: text, Author: author}, nil, false)
}

// Comments returns the issue's comments, oldest first; never nil.
func (c *Client) Comments(ctx context.Context, id string) ([]api.Comment, error) {
	var out api.CommentsResponse
	if err := c.do(ctx, http.MethodGet, issuePath(id, "comments"), nil, &out, true); err != nil {
		return nil, err
	}
	if out.Comments == nil {
		out.Comments = []api.Comment{}
	}
	return out.Comments, nil
}

// AddDependency records that issue depends on dependsOn with relation depType.
// dependsOn may be external:<rig>:<id>.
func (c *Client) AddDependency(ctx context.Context, issue, dependsOn, depType string) error {
	return c.do(ctx, http.MethodPost, issuePath(issue, "dependencies"), api.DependencyRequest{DependsOn: dependsOn, Type: depType}, nil, false)
}

// RemoveDependency removes the link from issue to dependsOn, whatever its
// type; a missing link is api.ErrNotFound.
func (c *Client) RemoveDependency(ctx context.Context, issue, dependsOn string) error {
	return c.do(ctx, http.MethodDelete, issuePath(issue, "dependencies")+"/"+url.PathEscape(dependsOn), nil, nil, false)
}

// Dependencies returns what id depends on with relation depType ("" for any);
// never nil.
func (c *Client) Dependencies(ctx context.Context, id, depType string) ([]api.IssueDep, error) {
	path := issuePath(id, "dependencies")
	if depType != "" {
		path += "?type=" + url.QueryEscape(depType)
	}
	var out api.DependenciesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out, true); err != nil {
		return nil, err
	}
	if out.Dependencies == nil {
		out.Dependencies = []api.IssueDep{}
	}
	return out.Dependencies, nil
}

func issuePath(id, tail string) string { return "/v1/issues/" + url.PathEscape(id) + "/" + tail }
