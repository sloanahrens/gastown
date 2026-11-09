package daemon

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jonboulle/clockwork"
)

// bootGateWaiting is the boot reason before the poller has run once. The gate
// fails closed: an answer nobody has read holds the background work the same
// as a probe that names itself not answering.
const bootGateWaiting = "waiting for infrastructure"

// bootGateProbeTimeout bounds each probe's answer within one poll.
const bootGateProbeTimeout = 10 * time.Second

// bootGateInterval is the wait between polls while some probe is not
// answering. The poller stops at the first healthy poll, so the wait only runs
// during the boot outage.
const bootGateInterval = 10 * time.Second

// bootGate runs the boot probe set off the heartbeat and keeps the last answer
// the poller read. standbyReason reads that answer, so the daemon's background
// work holds while infrastructure is not answering, and the lifecycle steps
// name the unanswering probes.
type bootGate struct {
	health *bootHealth

	// interval is the wait between polls while unhealthy. newBootGate sets
	// bootGateInterval; a test sets its own.
	interval time.Duration

	// clock times the wait between polls. newBootGate sets the real clock; a
	// test sets a fake one.
	clock clockwork.Clock

	// answer is the last reason the poller read: bootGateWaiting until the
	// first poll, then "" once every probe has answered.
	answer atomic.Pointer[string]
}

// newBootGate returns a gate over probes, waiting until the first poll.
func newBootGate(probes []bootProbe) (*bootGate, error) {
	health, err := newBootHealth(probes, bootGateProbeTimeout)
	if err != nil {
		return nil, err
	}
	g := &bootGate{
		health:   health,
		interval: bootGateInterval,
		clock:    clockwork.NewRealClock(),
	}
	waiting := bootGateWaiting
	g.answer.Store(&waiting)
	return g, nil
}

// reason is the boot reason standbyReason reads: the last answer the poller
// stored, which is bootGateWaiting until its first poll.
func (g *bootGate) reason() string {
	return *g.answer.Load()
}

// poll asks the probe set until every probe answers or ctx ends, storing each
// answer for reason. It logs once when the infrastructure answers. It runs on
// its own goroutine: the heartbeat never calls a probe. The wait before the
// next poll starts only after the answer is stored, so the stored answer is
// never behind the wait.
func (g *bootGate) poll(ctx context.Context, logf func(format string, v ...any)) {
	for {
		if ctx.Err() != nil {
			return
		}
		reason := g.health.Unhealthy(ctx)
		g.answer.Store(&reason)
		if reason == "" {
			logf("boot gate: infrastructure answering")
			return
		}
		timer := g.clock.NewTimer(g.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.Chan():
		}
	}
}
