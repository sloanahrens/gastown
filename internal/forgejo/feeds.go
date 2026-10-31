package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// userReposLimit bounds one page of the viewer's repository list. The list is
// read page by page to its end: a report built from the first page alone would
// silently leave every later repository out (gt-nlrpk).
const userReposLimit = 100

// userReposMaxPages bounds the paging loop. A server that serves a full page
// forever fails the read instead of spinning; 100 pages reaches past any
// town's repository count, and a server that served more is a fault to report,
// not a list to truncate.
const userReposMaxPages = 100

// Repository is the subset of a Forgejo repository the client reads: the
// owner/name an activity feed addresses the repo by, the size the nightly
// report charges storage for, and the reply to the probe's existence check
// (Client.Repository).
type Repository struct {
	FullName string `json:"full_name"`
	// SizeKB is the repository's size in kilobytes. It is a pointer because a
	// reply without the field is an unmeasured size, which is not zero bytes
	// (gt-nlrpk).
	SizeKB *int64 `json:"size"`
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

// ListUserRepos returns every repository the token's user can see, read page
// by page. It stops when a page comes back empty rather than when it is short:
// a server caps a page at its own maximum, so a short page may still have more
// behind it, while an empty one cannot (gt-nlrpk). Reading that closing empty
// page is why the panel's refresh makes one more call than the listing
// strictly needs.
func (c *Client) ListUserRepos(ctx context.Context) ([]Repository, error) {
	out := make([]Repository, 0, userReposLimit)
	for page := 1; page <= userReposMaxPages; page++ {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(userReposLimit))
		q.Set("page", strconv.Itoa(page))
		var rows []Repository
		if err := c.call(ctx, http.MethodGet, "/user/repos", q, nil, &rows); err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return out, nil
		}
		out = append(out, rows...)
	}
	return nil, fmt.Errorf("still full after %d pages", userReposMaxPages)
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
