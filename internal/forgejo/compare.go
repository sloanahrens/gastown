package forgejo

import (
	"context"
	"fmt"
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
//
// A body that carries no total_commits number is an error. The field can be
// absent, null, or in a body the client never decodes (an empty 200 or a 204),
// and each of those would otherwise leave the count at zero: the answer that
// says the head is already contained by the base. Refusing it keeps a missing
// count from being read as a fresh pin.
func (c *Client) CompareCommits(ctx context.Context, owner, repo, base, head string) (*CompareInfo, error) {
	// The pointer reads presence: an absent field, null and an undecoded body
	// all leave it nil, while a real count (zero included) fills it.
	var out struct {
		TotalCommits *int `json:"total_commits"`
	}
	path := repoPath(owner, repo, "/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head))
	if err := c.call(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	if out.TotalCommits == nil {
		return nil, fmt.Errorf("forgejo: compare %s...%s: response has no total_commits", base, head)
	}
	return &CompareInfo{TotalCommits: *out.TotalCommits}, nil
}
