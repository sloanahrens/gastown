package forgejo

import (
	"context"
	"net/http"
	"net/url"
)

// CommitState is a commit status's state, spelled as the API spells it.
type CommitState string

const (
	StatePending CommitState = "pending"
	StateSuccess CommitState = "success"
	StateError   CommitState = "error"
	StateFailure CommitState = "failure"
	StateWarning CommitState = "warning"
	StateSkipped CommitState = "skipped"
)

// StatusRequest is the body of a create-status call. Context is the name the
// branch protection matches, such as "ci / gate (push)".
type StatusRequest struct {
	State       CommitState `json:"state"`
	Context     string      `json:"context"`
	Description string      `json:"description,omitempty"`
	TargetURL   string      `json:"target_url,omitempty"`
}

// CommitStatus is one status posted against a commit.
type CommitStatus struct {
	ID          int64       `json:"id"`
	Status      CommitState `json:"status"`
	Context     string      `json:"context"`
	Description string      `json:"description"`
	TargetURL   string      `json:"target_url"`
	Creator     *User       `json:"creator,omitempty"`
}

// CombinedStatus is every status on a commit and their aggregate state.
type CombinedStatus struct {
	State      CommitState    `json:"state"`
	SHA        string         `json:"sha"`
	TotalCount int            `json:"total_count"`
	Statuses   []CommitStatus `json:"statuses"`
}

// PostStatus posts one commit status and returns it as stored.
func (c *Client) PostStatus(ctx context.Context, owner, repo, sha string, req StatusRequest) (*CommitStatus, error) {
	var out CommitStatus
	path := repoPath(owner, repo, "/statuses/"+url.PathEscape(sha))
	if err := c.call(ctx, http.MethodPost, path, nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CombinedStatus returns a commit's statuses and their aggregate state.
func (c *Client) CombinedStatus(ctx context.Context, owner, repo, ref string) (*CombinedStatus, error) {
	var out CombinedStatus
	path := repoPath(owner, repo, "/commits/"+url.PathEscape(ref)+"/status")
	if err := c.call(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StatusFor returns the status carrying context, or nil when the commit has
// none: an absent context is how the worker sees that CI has not reported.
func (s *CombinedStatus) StatusFor(context string) *CommitStatus {
	for i := range s.Statuses {
		if s.Statuses[i].Context == context {
			return &s.Statuses[i]
		}
	}
	return nil
}
