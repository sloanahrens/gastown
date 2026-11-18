package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/config"
)

// The workspace reconcile is the daemon's share of the bd gateway's workspace
// (docs/design/bd-container.md, "The workspace"). The daemon pushes the town's
// files when it starts and again on every tick, so a gateway that restarted or
// missed a push converges within one interval. It runs only when the daemon's
// environment names a gateway (BD_GATEWAY_URL): with none, the daemon starts and
// patrols exactly as before.

// startWorkspaceReconcile starts the reconcile when the patrol is enabled and a
// gateway is configured, and reports whether it did; Run ignores the answer, and
// the registration test asserts it. The loop runs on its own goroutine, so a slow
// or failed push never blocks the caller; a failed push is logged and retried at
// the next tick.
func (d *Daemon) startWorkspaceReconcile() bool {
	if !IsPatrolEnabled(d.patrol(), "workspace_reconcile") {
		return false
	}
	url := d.env(bdgateway.WorkspaceURLEnv)
	if url == "" {
		return false
	}
	drift := newWorkspaceDriftCheck()
	d.workspaceDrift = drift
	push := bdgateway.PushWorkspace
	if d.workspacePusher != nil {
		push = d.workspacePusher.Push
	} else {
		// The installed pusher is what every writer of the town copy pushes
		// through, so the reconcile and the writers share one push order.
		install := d.installWorkspacePusher
		if install == nil {
			install = bdgateway.SetWorkspacePusher
		}
		install(bdgateway.GatewayPusher{URL: url, TokenFile: d.env(bdgateway.WorkspaceTokenEnv), Observe: drift.observe})
	}
	go runWorkspaceReconcile(d.ctx, d.clk(), d.reconcileInterval, push, d.config.TownRoot, d.logger.Printf, drift)
	return true
}

// reconcileInterval returns the wait before the next push. It is read at each
// tick, so an edit to the patrol config takes effect at the next wait. A setting
// the config refuses is logged by name and the default is used: a bad interval
// never turns the reconcile off for a daemon that has a gateway.
func (d *Daemon) reconcileInterval() time.Duration {
	interval, err := d.patrol().WorkspaceReconcileInterval()
	if err != nil {
		d.logger.Printf("Workspace reconcile: %v; using the %s default", err, config.DefaultWorkspaceReconcileInterval)
		return config.DefaultWorkspaceReconcileInterval
	}
	return interval
}

// workspaceDriftGap is the least time after a push before a drift answer pushes
// again. A drift answer inside the gap waits for the gap to end.
const workspaceDriftGap = 10 * time.Second

// workspaceDrifted reports whether an answer says the gateway no longer serves the
// tree the last good push stored: a refusal that the workspace is not present, or
// a hash header other than lastGood. An answer with no header says nothing.
func workspaceDrifted(a bdgateway.WorkspaceAnswer, lastGood string) bool {
	if a.Absent {
		return true
	}
	return a.Hash != "" && a.Hash != lastGood
}

// workspaceDriftCheck carries the drift check from the gateway's answers to the
// reconcile loop. observe runs on whichever goroutine sends a push (the loop's own
// and a writer's), so it never waits on the loop. A drift answer sets signal, which
// holds one pending drift: a drift that finds one already pending adds nothing,
// because the pending push covers it, and no drift is lost.
type workspaceDriftCheck struct {
	mu       sync.Mutex
	lastGood string
	signal   chan struct{}
}

func newWorkspaceDriftCheck() *workspaceDriftCheck {
	return &workspaceDriftCheck{signal: make(chan struct{}, 1)}
}

// observe checks one gateway answer against the last good push and signals the
// loop when the answer shows drift.
func (c *workspaceDriftCheck) observe(a bdgateway.WorkspaceAnswer) {
	c.mu.Lock()
	drifted := workspaceDrifted(a, c.lastGood)
	c.mu.Unlock()
	if !drifted {
		return
	}
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

// stored records the hash of a push that stored: the tree answers are checked
// against from now on.
func (c *workspaceDriftCheck) stored(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastGood = hash
}

// runWorkspaceReconcile pushes the town at once, then once per interval, until
// ctx ends. A push that fails is logged and the next tick tries again; one failed
// answer does not end the loop.
//
// A drift signal pushes before the next tick: at once when the last push is at
// least workspaceDriftGap old, else at the end of that gap. Signals inside one gap
// share one push. Every push resets the tick and drops the deferred push, so the
// interval runs from the last push whatever sent it.
func runWorkspaceReconcile(ctx context.Context, clk clockwork.Clock, interval func() time.Duration, push func(context.Context, string) (string, error), townRoot string, logf func(string, ...any), drift *workspaceDriftCheck) {
	var (
		pushedAt  time.Time
		tick      clockwork.Timer
		deferred  clockwork.Timer
		deferredC <-chan time.Time
	)
	pushNow := func() {
		hash, err := push(ctx, townRoot)
		if err != nil {
			logf("Workspace reconcile: the bd gateway's workspace was not updated; retrying at the next tick: %v", err)
		} else {
			drift.stored(hash)
		}
		pushedAt = clk.Now()
		if deferred != nil {
			deferred.Stop()
		}
		deferred, deferredC = nil, nil
		if tick != nil {
			tick.Stop()
		}
		tick = clk.NewTimer(interval())
	}
	pushNow()
	defer func() {
		tick.Stop()
		if deferred != nil {
			deferred.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.Chan():
			pushNow()
		case <-deferredC:
			pushNow()
		case <-drift.signal:
			if deferred != nil {
				continue
			}
			wait := pushedAt.Add(workspaceDriftGap).Sub(clk.Now())
			if wait <= 0 {
				pushNow()
				continue
			}
			deferred = clk.NewTimer(wait)
			deferredC = deferred.Chan()
		}
	}
}
