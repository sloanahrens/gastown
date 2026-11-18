package daemon

// The outbox poll (docs/plans/2026-11-14-no-bead-access-plan.md, O4): a
// sandbox seat writes its outbox as it runs, and the seat-end copy-out reads it
// only when the seat ends. The poll gives the seat's escalation request a second
// route to the host while the seat still runs, so a seat blocked on a HELP does
// not wait for its own end to be heard (gt-0hkj0c).
//
// This file is the patrol's skeleton: its own ticker, off by default, and one
// tick seam the next bead fills with the read of each running seat's outbox and
// the escalation it files (gt-dq4ce6). Until then the seam is a no-op, so a
// town that turns the patrol on pays nothing.

import (
	"context"
	"time"
)

const (
	// outboxPollPatrol is the patrol's name in daemon.patrols, in
	// daemon.container_off and in the daemon audit table.
	outboxPollPatrol = "outbox_poll"

	// defaultOutboxPollInterval is the period between two polls: the plan gives
	// the patrol about ten minutes, so a seat's HELP waits at most one interval
	// from the seat's write rather than for the seat's end.
	defaultOutboxPollInterval = 10 * time.Minute

	// outboxPollTickTimeout bounds one tick, so a read that hangs frees the
	// patrol's single-flight guard at a known point. The next bead's per-seat
	// read keeps its own bound inside it (gt-dq4ce6).
	outboxPollTickTimeout = 2 * time.Minute

	// outboxPollJoinWait bounds how long Run waits for a tick still running
	// after the ticker stopped. A tick left past it finishes its own bounded
	// read, and the next start needs nothing it read.
	outboxPollJoinWait = 10 * time.Second
)

// outboxPollTickFunc is one tick: the read of every running seat's outbox, or
// the error that ended it. The daemon calls it once for each tick it runs, and
// it must return when ctx is done, so the tick's bound frees the patrol's
// single-flight guard.
type outboxPollTickFunc func(ctx context.Context) error

// outboxPollNoopTick is the seam's body until the outbox read lands: a tick
// that finds nothing to do, and so reports no error.
func outboxPollNoopTick(context.Context) error { return nil }

// startOutboxPollTicker starts the outbox poll's own ticker when the patrol is
// active, and returns a nil channel and stop func when it is not: a town that
// names no key starts nothing (gt-0hkj0c). The ticker's interval is
// defaultOutboxPollInterval.
func (d *Daemon) startOutboxPollTicker() (<-chan time.Time, func()) {
	if !d.isPatrolActive(outboxPollPatrol) {
		return nil, nil
	}
	newTicker := d.newOutboxPollTicker
	if newTicker == nil {
		newTicker = func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		}
	}
	ticks, stop := newTicker(defaultOutboxPollInterval)
	d.logger.Printf("Outbox poll ticker started (interval %v)", defaultOutboxPollInterval)
	return ticks, stop
}

// startOutboxPollTick runs one poll on its own goroutine, so a read that waits
// on the gateway holds that goroutine rather than the main select loop, which
// carries the heartbeat and every other patrol. A tick that arrives while the
// previous one still runs is skipped and logged, since two polls at once would
// read the same seat rows twice.
func (d *Daemon) startOutboxPollTick() {
	if !d.outboxPollRunning.CompareAndSwap(false, true) {
		d.logger.Printf("outbox_poll: previous tick still running, skipping")
		return
	}
	d.outboxPollCycles.Add(1)
	go func() {
		defer d.outboxPollCycles.Done()
		defer d.outboxPollRunning.Store(false)
		d.triggerOutboxPoll()
	}()
}

// triggerOutboxPoll runs one tick: the patrol's one seam, called once under
// outboxPollTickTimeout. A town that turned the patrol off since the ticker
// started reads nothing, and a seam that fails is logged rather than read as a
// quiet tick. A daemon built without a seam runs outboxPollNoopTick.
func (d *Daemon) triggerOutboxPoll() {
	if !d.isPatrolActive(outboxPollPatrol) {
		return
	}
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, outboxPollTickTimeout)
	defer cancel()
	tick := d.outboxPollTick
	if tick == nil {
		tick = outboxPollNoopTick
	}
	if err := tick(ctx); err != nil {
		d.logger.Printf("outbox_poll: %v", err)
	}
}

// stopOutboxPollPatrol stops the patrol's ticker and joins the tick it may have
// running, bounded, so Run's return leaves no tick reading the town after the
// daemon is gone. A tick still running at the bound is logged and left: its
// read is bounded by outboxPollTickTimeout, and a shutdown does not wait for
// one.
func (d *Daemon) stopOutboxPollPatrol(stop func()) {
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		d.outboxPollCycles.Wait()
		close(done)
	}()
	bound := d.outboxPollJoinWait
	if bound <= 0 {
		bound = outboxPollJoinWait
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		d.logger.Printf("outbox_poll: a tick is still running %s after the ticker stopped; Run leaves without it", bound)
	}
}
