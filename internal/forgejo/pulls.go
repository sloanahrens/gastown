package forgejo

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// openPullsLimit bounds the page of open pull requests the landing reads when
// it is looking for one it already opened.
const openPullsLimit = 50

// PullRequest is a Forgejo pull request as the landing path reads it.
type PullRequest struct {
	ID             int64        `json:"id"`
	Number         int64        `json:"number"`
	Title          string       `json:"title"`
	Body           string       `json:"body"`
	State          string       `json:"state"`
	Merged         bool         `json:"merged"`
	Mergeable      bool         `json:"mergeable"`
	MergeCommitSHA string       `json:"merge_commit_sha"`
	HTMLURL        string       `json:"html_url"`
	Head           PRBranchInfo `json:"head"`
	Base           PRBranchInfo `json:"base"`
	User           *User        `json:"user,omitempty"`
}

// PRBranchInfo names the branch and commit one end of a pull request points
// at; Head.SHA is the commit a merge's head_commit_id must match.
type PRBranchInfo struct {
	Label  string `json:"label"`
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
	RepoID int64  `json:"repo_id"`
}

// CreatePullRequestOption is the body of a create-pull-request call.
type CreatePullRequestOption struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

// MergeStyle is how a merge combines the head into the base.
type MergeStyle string

const (
	MergeStyleMerge       MergeStyle = "merge"
	MergeStyleRebase      MergeStyle = "rebase"
	MergeStyleSquash      MergeStyle = "squash"
	MergeStyleFastForward MergeStyle = "fast-forward-only"
)

// MergePullRequestOption is the body of a merge call. Style carries the
// API form's "Do" key; HeadCommitID voids the merge when the head moved after
// the verdict.
type MergePullRequestOption struct {
	Style                  MergeStyle `json:"Do"`
	HeadCommitID           string     `json:"head_commit_id,omitempty"`
	Message                string     `json:"MergeMessageField,omitempty"`
	DeleteBranchAfterMerge bool       `json:"delete_branch_after_merge,omitempty"`
	MergeWhenChecksSucceed bool       `json:"merge_when_checks_succeed,omitempty"`
}

// CreatePull opens a pull request and returns it.
func (c *Client) CreatePull(ctx context.Context, owner, repo string, opt CreatePullRequestOption) (*PullRequest, error) {
	var out PullRequest
	if err := c.call(ctx, http.MethodPost, repoPath(owner, repo, "/pulls"), nil, opt, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpenPulls returns the repository's open pull requests, one bounded page.
// The landing reads it to find the PR it opened for a candidate branch: when
// the outdated-branch guard refuses a merge the branch is rebuilt, and its PR
// follows the branch, so the retry merges that PR at its new head instead of
// failing to open a second one for the same pair of branches.
func (c *Client) OpenPulls(ctx context.Context, owner, repo string) ([]PullRequest, error) {
	q := url.Values{}
	q.Set("state", "open")
	q.Set("limit", strconv.Itoa(openPullsLimit))
	var out []PullRequest
	if err := c.call(ctx, http.MethodGet, repoPath(owner, repo, "/pulls"), q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetPull returns the pull request numbered index.
func (c *Client) GetPull(ctx context.Context, owner, repo string, index int64) (*PullRequest, error) {
	var out PullRequest
	path := repoPath(owner, repo, "/pulls/"+strconv.FormatInt(index, 10))
	if err := c.call(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MergePull merges the pull request. A 409 means the base branch moved and is
// the worker's cue to rebuild the candidate; nothing tells it apart from a
// merge error, so it checks *APIError.IsConflict.
func (c *Client) MergePull(ctx context.Context, owner, repo string, index int64, opt MergePullRequestOption) error {
	path := repoPath(owner, repo, "/pulls/"+strconv.FormatInt(index, 10)+"/merge")
	return c.call(ctx, http.MethodPost, path, nil, opt, nil)
}
