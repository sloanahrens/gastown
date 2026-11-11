package forgejo

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Branch is the part of a branch the API answers with that a reader needs: the
// branch's name and the full commit it points at.
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

// ListBranches reads one page of a repository's branches, pages counted from 1
// and limit branches to a page. The caller follows the pages until one comes
// back short.
func (c *Client) ListBranches(ctx context.Context, owner, repo string, page, limit int) ([]Branch, error) {
	var out []Branch
	query := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
	if err := c.call(ctx, http.MethodGet, repoPath(owner, repo, "/branches"), query, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteBranch deletes a branch through the API.
//
// The name travels as one escaped path segment. Forgejo registers the branch
// route as a wildcard and reads the name from it without unescaping, so a name
// carrying "/" — every land/<bead> — must arrive as %2F: against Forgejo
// 16.0.5 the escaped form deletes (204) and the literal slash does not (500,
// gt-fn9e6.21).
func (c *Client) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	path := repoPath(owner, repo, "/branches/"+url.PathEscape(branch))
	return c.call(ctx, http.MethodDelete, path, nil, nil, nil)
}
