// Package runnerroll holds the steps of the runner roll. This file is the wait
// for the landing worker to go quiet, so a roll can take the landing hold and
// know that no pass is still in flight.
package runnerroll

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// RollPause is the landing-hold reason the roll takes. A pause with any other
// reason is someone else's hold, and the wait keeps polling through it.
const RollPause = "runner roll"

// pollInterval is how often the health reading is read while the wait runs.
const pollInterval = 5 * time.Second

// ErrWaitTimeout is returned when the landing passes are still in flight at the
// bound. The wrapped error names the passes count.
var ErrWaitTimeout = errors.New("landing wait timed out")

// Clock is the wait's time source. A fake advances only on Sleep, so the poll
// loop runs without waiting in tests.
type Clock interface {
	Now() time.Time
	// Sleep waits d, or until ctx is done, whichever comes first.
	Sleep(ctx context.Context, d time.Duration)
}

// WaitLandingIdle polls the health reading every pollInterval until the landing
// hold is the roll's own and no landing pass is in flight, then returns nil. It
// prints one line each time the pause or the passes count changes.
//
// It never reports idle on a reading it cannot trust: a read error is returned,
// and a reading whose passes count is unknown (a daemon that predates the
// field) is an error. A wait still not idle when bound passes returns an error
// wrapping ErrWaitTimeout that names the passes count.
func WaitLandingIdle(ctx context.Context, read func(ctx context.Context) (pause string, passes int, known bool, err error), clock Clock, bound time.Duration, out io.Writer) error {
	deadline := clock.Now().Add(bound)
	var printed bool
	var lastPause string
	var lastPasses int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pause, passes, known, err := read(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return fmt.Errorf("read landing health: %w", err)
		}
		if !known {
			return errors.New("landing passes unknown: the daemon predates the landing_passes field, so the wait cannot tell a quiet town from a busy one")
		}
		if !printed || pause != lastPause || passes != lastPasses {
			fmt.Fprintf(out, "landing: pause %q, %d passes in flight\n", pause, passes)
			printed, lastPause, lastPasses = true, pause, passes
		}
		if pause == RollPause && passes == 0 {
			return nil
		}
		now := clock.Now()
		if !now.Before(deadline) {
			return fmt.Errorf("%w: %d landing passes still in flight after %s", ErrWaitTimeout, passes, bound)
		}
		clock.Sleep(ctx, min(deadline.Sub(now), pollInterval))
	}
}
