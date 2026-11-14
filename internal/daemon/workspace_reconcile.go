package daemon

import (
	"context"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// The workspace reconcile is the daemon's share of the bd gateway's workspace
// (docs/design/bd-container.md, "The workspace"). The daemon pushes the town's
// files when it starts and again on every tick, so a gateway that restarted or
// missed a push converges within one interval. It runs only when the daemon's
// environment names a gateway (BD_GATEWAY_URL): with none, the daemon starts and
// patrols exactly as before.

// startWorkspaceReconcile starts the reconcile when a gateway is configured. The
// ticker is created here, before the loop goroutine starts, so no tick is lost
// to a race with startup. A failed push is logged and retried at the next tick;
// it never blocks the caller.
func (d *Daemon) startWorkspaceReconcile() {
	url := d.env(bdgateway.WorkspaceURLEnv)
	if url == "" {
		return
	}
	interval, err := d.patrol().WorkspaceReconcileInterval()
	if err != nil {
		d.logger.Printf("Workspace reconcile not started: %v", err)
		return
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
	ticks := d.clk().NewTicker(interval)
	go runWorkspaceReconcile(d.ctx, ticks, push, d.config.TownRoot, d.logger.Printf)
}

// runWorkspaceReconcile pushes the town at once, then once per tick, until ctx
// ends. A push that fails is logged and the next tick tries again; one failed
// answer does not end the loop.
func runWorkspaceReconcile(ctx context.Context, ticks clockwork.Ticker, push func(context.Context, string) (string, error), townRoot string, logf func(string, ...any)) {
	defer ticks.Stop()
	for {
		if _, err := push(ctx, townRoot); err != nil {
			logf("Workspace reconcile: the bd gateway's workspace was not updated; retrying at the next tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks.Chan():
		}
	}
}
