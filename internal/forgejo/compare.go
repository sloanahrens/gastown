package forgejo

import (
	"context"
	"net/http"
	"net/url"
)

// CompareInfo is the API's answer to a comparison between two commits.
//
// TotalCommits counts the commits the head has that the base does not, so a
// zero answer puts the head at or behind the base: the head is an ancestor of
// the base, or the two are the same commit. The ancillary commits and changed
// files are not read — asking the ancestry question this way is what lets the
// landing path answer "does this run contain that landing?" without a local
// clone of the app rig's history (verified live against Forgejo 16.0.5).
type CompareInfo struct {
	TotalCommits int `json:"total_commits"`
}

// CompareCommits compares base and head, the two commit-ish names the API
// accepts.
func (c *Client) CompareCommits(ctx context.Context, owner, repo, base, head string) (*CompareInfo, error) {
	var out CompareInfo
	path := repoPath(owner, repo, "/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head))
	if err := c.call(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
