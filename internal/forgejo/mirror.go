package forgejo

import (
	"context"
	"net/http"
	"time"
)

// PushMirror is a repository's push mirror and the result of its last sync.
// The monitor alerts on LastError and on a LastUpdate older than its window.
type PushMirror struct {
	RemoteName    string    `json:"remote_name"`
	RemoteAddress string    `json:"remote_address"`
	BranchFilter  string    `json:"branch_filter"`
	SyncOnCommit  bool      `json:"sync_on_commit"`
	Interval      string    `json:"interval"`
	LastUpdate    time.Time `json:"last_update"`
	LastError     string    `json:"last_error"`
	PublicKey     string    `json:"public_key"`
}

// CreatePushMirrorOption is the body of a create-push-mirror call. UseSSH
// takes the deploy key Forgejo generates, which is the GitHub mirror's
// credential of record.
type CreatePushMirrorOption struct {
	RemoteAddress string `json:"remote_address"`
	UseSSH        bool   `json:"use_ssh,omitempty"`
	BranchFilter  string `json:"branch_filter,omitempty"`
	SyncOnCommit  bool   `json:"sync_on_commit,omitempty"`
	Interval      string `json:"interval,omitempty"`
}

// ListPushMirrors returns the repository's push mirrors.
func (c *Client) ListPushMirrors(ctx context.Context, owner, repo string) ([]PushMirror, error) {
	var out []PushMirror
	if err := c.call(ctx, http.MethodGet, repoPath(owner, repo, "/push_mirrors"), nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreatePushMirror adds a push mirror and returns it.
func (c *Client) CreatePushMirror(ctx context.Context, owner, repo string, opt CreatePushMirrorOption) (*PushMirror, error) {
	var out PushMirror
	if err := c.call(ctx, http.MethodPost, repoPath(owner, repo, "/push_mirrors"), nil, opt, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
