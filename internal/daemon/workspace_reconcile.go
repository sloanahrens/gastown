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
	var probe func(context.Context) error
	if d.workspacePusher != nil {
		push = d.workspacePusher.Push
	} else {
		// The installed pusher is what every writer of the town copy pushes
		// through, so the reconcile and the writers share one push order. The probe
		// is the same pusher's GET, so it reaches the gateway the same way.
		gateway := bdgateway.GatewayPusher{URL: url, TokenFile: d.env(bdgateway.WorkspaceTokenEnv), HTTP: d.workspaceHTTP, Observe: drift.observe}
		install := d.installWorkspacePusher
		if install == nil {
			install = bdgateway.SetWorkspacePusher
		}
		install(gateway)
		probe = gateway.Probe
	}
	go runWorkspaceReconcile(d.ctx, d.clk(), d.reconcileInterval, push, probe, d.config.TownRoot, d.logger.Printf, drift)
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

// workspaceAbsentFloor is the least time between two pushes that an absent refusal
// triggered. A gateway that keeps refusing as absent gets one push per floor, not
// one per drift gap.
const workspaceAbsentFloor = 30 * time.Second

// workspaceProbeInterval is the wait between the loop's probes of the gateway. A
// probe is one GET that reads the gateway's hash header, so a gateway restarted
// with an empty workspace is seen within this interval, not at the next push tick.
const workspaceProbeInterval = 10 * time.Second

// workspaceDrifted reports whether an answer says the gateway no longer serves the
// tree the last good push stored: a refusal that the workspace is not present, or
// a hash header other than lastGood.
func workspaceDrifted(a bdgateway.WorkspaceAnswer, lastGood string) bool {
	if a.Absent {
		return true
	}
	// An answer with no header says nothing about the tree, so it is not drift.
	if a.Hash == "" {
		return false
	}
	return a.Hash != lastGood
}

// workspaceDriftCheck carries the drift check from the gateway's answers to the
// reconcile loop. observe runs on whichever goroutine sends a push (the loop's own
// and a writer's) or a probe, so it never waits on the loop. An answer to the loop's
// own push is never drift: that push wrote the tree the answer echoes, and it arrives before
// stored records the new hash. A drift answer is recorded as pending and wakes the
// loop through signal; take reads everything pending in one step, so drifts that
// arrive before the loop reads share one push and none is lost.
type workspaceDriftCheck struct {
	mu            sync.Mutex
	lastGood      string
	pending       bool
	pendingAbsent bool
	signal        chan struct{}
}

func newWorkspaceDriftCheck() *workspaceDriftCheck {
	return &workspaceDriftCheck{signal: make(chan struct{}, 1)}
}

// observe checks one gateway answer against the last good push and records a
// drift when the answer shows one that is not the loop's own.
func (c *workspaceDriftCheck) observe(a bdgateway.WorkspaceAnswer) {
	c.mu.Lock()
	if a.Own || !workspaceDrifted(a, c.lastGood) {
		c.mu.Unlock()
		return
	}
	c.pending = true
	c.pendingAbsent = c.pendingAbsent || a.Absent
	c.mu.Unlock()
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

// take clears the drifts pending since the last take. It reports whether any were
// pending, and whether one of them is an absent refusal.
func (c *workspaceDriftCheck) take() (pending, absent bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pending, absent = c.pending, c.pendingAbsent
	c.pending, c.pendingAbsent = false, false
	return pending, absent
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
// When probe is not nil, the loop also probes the gateway every
// workspaceProbeInterval. The probe's answer goes through the same drift check as
// a push's answer, so a gateway that lost its workspace is seen within one probe
// interval, whatever sent the daemon's last push. A probe that gets no answer is
// logged when it starts failing and when it answers again, and triggers no push.
//
// A drift signal pushes before the next tick: at once when the last push is at
// least workspaceDriftGap old, else at the end of that gap. A drift that an absent
// refusal caused also waits until workspaceAbsentFloor has passed since the last
// push an absent refusal caused. Signals inside one wait share one push. Every push
// resets the tick and drops the deferred push, so the interval runs from the last
// push whatever sent it.
func runWorkspaceReconcile(ctx context.Context, clk clockwork.Clock, interval func() time.Duration, push func(context.Context, string) (string, error), probe func(context.Context) error, townRoot string, logf func(string, ...any), drift *workspaceDriftCheck) {
	var (
		pushedAt       time.Time
		absentAt       time.Time
		tick           clockwork.Timer
		deferred       clockwork.Timer
		deferredC      <-chan time.Time
		deferredDue    time.Time
		deferredAbsent bool
		probeTimer     clockwork.Timer
		probeC         <-chan time.Time
		probeDown      bool
	)
	// A nil probe leaves probeC nil, so the select never takes that case.
	if probe != nil {
		probeTimer = clk.NewTimer(workspaceProbeInterval)
		probeC = probeTimer.Chan()
	}
	// probeOnce asks the gateway for its answer and logs only a change of state:
	// the first failure after an answer, and the first answer after failures.
	probeOnce := func() {
		err := probe(ctx)
		switch {
		case err != nil && !probeDown:
			probeDown = true
			logf("Workspace reconcile: the bd gateway did not answer the workspace probe; probing again every %s: %v", workspaceProbeInterval, err)
		case err == nil && probeDown:
			probeDown = false
			logf("Workspace reconcile: the bd gateway answers the workspace probe again")
		}
	}
	// pushNow pushes the town as the loop's own push. absent says the push answers
	// an absent refusal, so it starts the absent floor.
	pushNow := func(absent bool) {
		hash, err := push(bdgateway.AsOwnPush(ctx), townRoot)
		if err != nil {
			logf("Workspace reconcile: the bd gateway's workspace was not updated; retrying at the next tick: %v", err)
		} else {
			drift.stored(hash)
		}
		pushedAt = clk.Now()
		if absent {
			absentAt = pushedAt
		}
		if deferred != nil {
			deferred.Stop()
		}
		deferred, deferredC, deferredAbsent = nil, nil, false
		if tick != nil {
			tick.Stop()
		}
		tick = clk.NewTimer(interval())
	}
	pushNow(false)
	defer func() {
		tick.Stop()
		if deferred != nil {
			deferred.Stop()
		}
		if probeTimer != nil {
			probeTimer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.Chan():
			pushNow(false)
		case <-probeC:
			// The probe runs before its timer is re-armed, so a clock wait that sees the
			// timer again knows the probe's answer is already observed.
			probeOnce()
			probeTimer = clk.NewTimer(workspaceProbeInterval)
			probeC = probeTimer.Chan()
		case <-deferredC:
			pushNow(deferredAbsent)
		case <-drift.signal:
			pending, absent := drift.take()
			if !pending {
				continue
			}
			absent = absent || deferredAbsent
			due := pushedAt.Add(workspaceDriftGap)
			if absent && absentAt.Add(workspaceAbsentFloor).After(due) {
				due = absentAt.Add(workspaceAbsentFloor)
			}
			if deferred != nil && !due.After(deferredDue) {
				// The deferred push is due no later than this drift needs, so it covers it.
				deferredAbsent = absent
				continue
			}
			now := clk.Now()
			if !due.After(now) {
				pushNow(absent)
				continue
			}
			if deferred != nil {
				deferred.Stop()
			}
			deferred = clk.NewTimer(due.Sub(now))
			deferredC = deferred.Chan()
			deferredDue = due
			deferredAbsent = absent
		}
	}
}
