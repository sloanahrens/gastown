package daemon

import (
	"context"
	"log"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// gateRunThrottleTimeout bounds the runner throttle read on the landing path.
const gateRunThrottleTimeout = 5 * time.Second

// NewGateRunRecorder returns the landing worker's gate run recorder, which fills the row's runner throttle counters and appends it, writing -1 for all three when the read fails or passes its deadline.
func NewGateRunRecorder(exec func(context.Context, ...string) (string, error), townRoot string, logf func(string, ...any)) func(land.GateRun) {
	return newGateRunRecorder(exec, townRoot, logf, gateRunThrottleTimeout)
}

// newGateRunRecorder is NewGateRunRecorder with the throttle deadline injected. A failed read and a failed append are each logged, and neither blocks the caller.
func newGateRunRecorder(exec func(context.Context, ...string) (string, error), townRoot string, logf func(string, ...any), throttleTimeout time.Duration) func(land.GateRun) {
	if logf == nil {
		logf = log.Printf
	}
	return func(run land.GateRun) {
		run.RunnerPeriods, run.RunnerThrottled, run.RunnerThrottledUsec = runnerCounters(exec, logf, throttleTimeout)
		if err := land.AppendGateRun(townRoot, run); err != nil {
			logf("gate run recorder: appending the run for %s: %v", run.Bead, err)
		}
	}
}

// runnerCounters reads the runner throttle under throttleTimeout, returning -1
// for each counter when the read fails or outlives the bound: -1 marks a reading
// not taken and never reads as a real zero. The read runs beside the bound, so a
// reader that ignores the context is abandoned, not waited on.
func runnerCounters(exec func(context.Context, ...string) (string, error), logf func(string, ...any), throttleTimeout time.Duration) (periods, throttled, throttledUsec int64) {
	ctx, cancel := context.WithTimeout(context.Background(), throttleTimeout)
	defer cancel()
	type reading struct {
		t   RunnerThrottle
		err error
	}
	read := make(chan reading, 1) // buffered: an abandoned read can still end
	go func() {
		t, err := ReadRunnerThrottle(ctx, exec)
		read <- reading{t, err}
	}()
	select {
	case r := <-read:
		if r.err != nil {
			logf("gate run recorder: reading the runner throttle: %v", r.err)
			return -1, -1, -1
		}
		return r.t.Periods, r.t.Throttled, r.t.ThrottledUsec
	case <-ctx.Done():
		logf("gate run recorder: the runner throttle read abandoned at its %s bound: %v", throttleTimeout, ctx.Err())
		return -1, -1, -1
	}
}
