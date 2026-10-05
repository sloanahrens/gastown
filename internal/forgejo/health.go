package forgejo

import (
	"context"
	"net/http"
)

// This file holds the calls the town health probe makes (gt-fn9e6.11): the
// instance's version, which needs no token, and the runners registered to a
// repository, which need the landing bot's. They are the two questions that
// tell an operator the landing path is about to go quiet rather than slow.

// ServerVersion is GET /version's body.
type ServerVersion struct {
	Version string `json:"version"`
}

// Version reads the instance's version. It is the unauthenticated liveness
// check: any 2xx answer means the instance the landing path pushes to and
// waits on is up, whatever version it is.
func (c *Client) Version(ctx context.Context) (*ServerVersion, error) {
	var out ServerVersion
	if err := c.call(ctx, http.MethodGet, "/version", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ActionRunner is one registered CI runner, as the runner API reports it.
type ActionRunner struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Labels []string `json:"labels,omitempty"`
}

// The statuses the API reports for a runner. There is no "online": a runner
// that is up is idle or active, and only "offline" is down (Forgejo 16,
// ActionRunner.status).
const (
	RunnerOffline = "offline"
	RunnerIdle    = "idle"
	RunnerActive  = "active"
)

// Online reports whether the runner is registered and reachable. An unknown
// status is not online: a status this code does not know is not evidence that
// CI can pick a run up.
func (r ActionRunner) Online() bool {
	return r.Status == RunnerIdle || r.Status == RunnerActive
}

// ListRepoRunners returns the runners registered to owner/repo, the ones a
// candidate pushed there can be picked up by. The body is the array of
// runners itself (Forgejo 16); the total rides in a header, which is not
// needed: a rig's runner list is short, and online among what came back is
// the whole question.
func (c *Client) ListRepoRunners(ctx context.Context, owner, repo string) ([]ActionRunner, error) {
	var out []ActionRunner
	if err := c.call(ctx, http.MethodGet, repoPath(owner, repo, "/actions/runners"), nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
