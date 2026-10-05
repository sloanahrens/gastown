package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// This file holds the calls the town health probe makes (gt-fn9e6.11): the
// instance's version, which needs no token, and the state of the run queue the
// landing path waits on being picked up.

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

// The two statuses a workflow run carries before a runner picks it up: Forgejo
// reports a run this way until something starts it (Forgejo 16).
const (
	RunStatusWaiting = "waiting"
	RunStatusBlocked = "blocked"
)

// RunPickup is the oldest workflow run waiting to be picked up: the run queue
// read that answers whether CI is taking work.
//
// The runner listing cannot answer it. Our runner is registered at the
// instance level, so no repository listing contains it, and Forgejo answers
// 403 to the landing bot on the repository runner API — the probe read "none
// online" for a runner that was picking runs up (gt-fn9e6.56). The run queue
// is readable with the same token, and a wait in it is the state an operator
// cares about: work is not moving.
type RunPickup struct {
	// Waiting reports whether a run is waiting to be picked up at all.
	Waiting bool
	// Since is the oldest waiting run's creation time: when it began waiting.
	Since time.Time
	// ID is that run's id, for the health line's detail.
	ID int64
}

// waitRunPage bounds one run list request. A stalled queue holds few runs —
// each waiting run is one landing attempt — so the oldest is on the first page
// long before its age crosses the red threshold.
const waitRunPage = 50

// WaitingRun reads the oldest run waiting to be picked up in owner/repo.
//
// A run is waiting while its status is waiting or blocked and it carries no
// start time: a run a runner has started is not in the queue, whatever its
// status says.
func (c *Client) WaitingRun(ctx context.Context, owner, repo string) (RunPickup, error) {
	list, err := c.ListRuns(ctx, owner, repo, RunFilter{
		Status: []string{RunStatusWaiting, RunStatusBlocked},
		Limit:  waitRunPage,
	})
	if err != nil {
		return RunPickup{}, err
	}
	var oldest RunPickup
	for _, run := range list.Runs {
		if !waitingRun(run) {
			continue
		}
		created, err := time.Parse(time.RFC3339, run.Created)
		if err != nil {
			return RunPickup{}, fmt.Errorf("run %d has an unreadable created time %q", run.ID, run.Created)
		}
		if !oldest.Waiting || created.Before(oldest.Since) {
			oldest = RunPickup{Waiting: true, Since: created, ID: run.ID}
		}
	}
	return oldest, nil
}

// waitingRun reports whether run is still in the queue. Forgejo sends the zero
// timestamp for a run no runner has started, so a start time that does not
// parse, or that reads as the epoch, is no start.
func waitingRun(run ActionRun) bool {
	if run.Status != RunStatusWaiting && run.Status != RunStatusBlocked {
		return false
	}
	started, err := time.Parse(time.RFC3339, run.Started)
	if err != nil {
		return true
	}
	return !started.After(time.Unix(0, 0))
}
