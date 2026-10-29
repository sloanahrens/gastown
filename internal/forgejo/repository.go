package forgejo

import (
	"context"
	"net/http"
)

// Repository returns the repository the owner/name pair addresses.
//
// It is the probe's existence check: a repository that is not on the instance
// answers as the package's *APIError with StatusCode http.StatusNotFound, so a
// caller tells a missing repo from a transport or auth failure and refuses the
// run rather than reading either as absent (gt-tpg0p.86).
func (c *Client) Repository(ctx context.Context, owner, repo string) (*Repository, error) {
	var out Repository
	if err := c.call(ctx, http.MethodGet, repoPath(owner, repo, ""), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
