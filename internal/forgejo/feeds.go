package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// userReposLimit bounds the viewer's repository list. The dashboard reads a
// handful of repos, so one page is always the whole list.
const userReposLimit = 100

// Repository is the subset of a Forgejo repository the activity feed carries:
// the owner/name the feed addresses the repo by.
type Repository struct {
	FullName string `json:"full_name"`
}

// Activity is one entry of a repository's activity feed, the list Forgejo's
// homepage draws. Content is a JSON blob whose shape depends on OpType, and is
// empty on the rows Forgejo writes without one.
type Activity struct {
	ID      int64       `json:"id"`
	OpType  string      `json:"op_type"`
	ActUser *User       `json:"act_user,omitempty"`
	Repo    *Repository `json:"repo,omitempty"`
	RefName string      `json:"ref_name"`
	Content string      `json:"content"`
	Created time.Time   `json:"created"`
}

// PushCommit is one commit inside a commit_repo activity's content blob.
type PushCommit struct {
	Message string `json:"Message"`
}

// PushContent is the content blob a commit_repo activity carries: the commits
// the push listed, and its head.
type PushContent struct {
	Commits    []PushCommit `json:"Commits"`
	HeadCommit *PushCommit  `json:"HeadCommit"`
}

// HeadMessage returns the subject line of the push's head commit — the
// HeadCommit the blob names, or the newest commit when it names none. ok is
// false when the content carries no commit message, which happens for a
// branch delete and for the duplicate row Forgejo writes beside a push
// (verified against 16.0.5).
func (a Activity) HeadMessage() (string, bool) {
	var c PushContent
	if a.Content == "" || json.Unmarshal([]byte(a.Content), &c) != nil {
		return "", false
	}
	commit := c.HeadCommit
	if commit == nil && len(c.Commits) > 0 {
		commit = &c.Commits[len(c.Commits)-1]
	}
	if commit == nil {
		return "", false
	}
	subject := strings.TrimSpace(strings.SplitN(commit.Message, "\n", 2)[0])
	return subject, subject != ""
}

// ListUserRepos returns the repositories the token's user can see.
func (c *Client) ListUserRepos(ctx context.Context) ([]Repository, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(userReposLimit))
	var out []Repository
	if err := c.call(ctx, http.MethodGet, "/user/repos", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListRepoActivities returns a repository's recent activities, newest first.
func (c *Client) ListRepoActivities(ctx context.Context, owner, repo string, limit int) ([]Activity, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []Activity
	if err := c.call(ctx, http.MethodGet, repoPath(owner, repo, "/activities/feeds"), q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
