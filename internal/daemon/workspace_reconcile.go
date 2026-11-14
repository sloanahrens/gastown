package daemon

import (
	"context"
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

// startWorkspaceReconcile starts the reconcile when a gateway is configured, and
// reports whether it did. The caller's start-up list gains the reconcile exactly
// when this returns true. The loop runs on its own goroutine, so a slow or failed
// push never blocks the caller; a failed push is logged and retried at the next
// tick.
func (d *Daemon) startWorkspaceReconcile() bool {
	url := d.env(bdgateway.WorkspaceURLEnv)
	if url == "" {
		return false
	}
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
		install(bdgateway.GatewayPusher{URL: url, TokenFile: d.env(bdgateway.WorkspaceTokenEnv)})
	}
	go runWorkspaceReconcile(d.ctx, d.clk(), d.reconcileInterval, push, d.config.TownRoot, d.logger.Printf)
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

// runWorkspaceReconcile pushes the town at once, then once per interval, until
// ctx ends. A push that fails is logged and the next tick tries again; one failed
// answer does not end the loop.
func runWorkspaceReconcile(ctx context.Context, clk clockwork.Clock, interval func() time.Duration, push func(context.Context, string) (string, error), townRoot string, logf func(string, ...any)) {
	for {
		if _, err := push(ctx, townRoot); err != nil {
			logf("Workspace reconcile: the bd gateway's workspace was not updated; retrying at the next tick: %v", err)
		}
		timer := clk.NewTimer(interval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.Chan():
		}
	}
}
