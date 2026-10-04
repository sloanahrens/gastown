package forgejo

import (
	"context"
	"net/http"
	"net/url"
)

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
