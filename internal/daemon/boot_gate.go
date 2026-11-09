package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/dispatch"
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

	// townRoot is where the poller publishes the reason (dispatch.BootHealthPath),
	// so the dispatch and sling callers in other processes see the hold.
	townRoot string

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

// newBootGate returns a gate over probes that publishes its reason under
// townRoot, waiting until the first poll. The only way newBootHealth fails is a
// timeout that is not positive, and bootGateProbeTimeout is a positive constant,
// so a failure here is a coding error caught at the first start, not a runtime
// condition to handle.
func newBootGate(townRoot string, probes []bootProbe) *bootGate {
	health, err := newBootHealth(probes, bootGateProbeTimeout)
	if err != nil {
		panic(fmt.Sprintf("boot gate: %v", err))
	}
	g := &bootGate{
		health:   health,
		townRoot: townRoot,
		interval: bootGateInterval,
		clock:    clockwork.NewRealClock(),
	}
	waiting := bootGateWaiting
	g.answer.Store(&waiting)
	return g
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
//
// The reason is also published to the boot-health file for the dispatchers in
// other processes: the waiting reason before the first poll, then each changed
// reason, and the file is removed at the first healthy answer. A publish failure
// is logged and leaves the in-process answer as it is, so the daemon's
// background work still holds.
func (g *bootGate) poll(ctx context.Context, logf func(format string, v ...any)) {
	published := ""
	if err := g.publish(bootGateWaiting); err != nil {
		logf("boot gate: publishing %q: %v", bootGateWaiting, err)
	} else {
		published = bootGateWaiting
	}
	for {
		if ctx.Err() != nil {
			return
		}
		reason := g.health.Unhealthy(ctx)
		g.answer.Store(&reason)
		if reason == "" {
			if err := g.unpublish(); err != nil {
				logf("boot gate: removing the boot hold file: %v", err)
			}
			logf("boot gate: infrastructure answering")
			return
		}
		if reason != published {
			if err := g.publish(reason); err != nil {
				logf("boot gate: publishing %q: %v", reason, err)
			} else {
				published = reason
			}
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

// publish writes reason, as one line, to the boot-health file. The write is
// atomic, so a dispatcher reads either the previous reason or this one.
func (g *bootGate) publish(reason string) error {
	path := dispatch.BootHealthPath(g.townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, []byte(reason+"\n"), 0o644)
}

// unpublish removes the boot-health file. A file that is already gone is not an
// error.
func (g *bootGate) unpublish() error {
	err := os.Remove(dispatch.BootHealthPath(g.townRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
