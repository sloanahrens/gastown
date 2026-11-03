package update

import (
	"context"
	"time"
)

// Seat is one polecat seat as the drain sees it: whether its session reports
// working and when it last did anything.
type Seat struct {
	Name       string
	Working    bool
	LastActive time.Time
}

// SeatProbe reads the town's seats. It offers no way to change a seat, so a
// drain built on it cannot stop, interrupt or kill one.
type SeatProbe interface {
	Seats(ctx context.Context) ([]Seat, error)
}

// DrainOptions bounds a drain. The caller supplies the defaults 30 minutes, 5
// minutes and 15 seconds.
type DrainOptions struct {
	// Timeout is how long the drain may wait, measured from the first check.
	Timeout time.Duration
	// IdleAfter is how long a working seat may be silent before it counts as
	// drained.
	IdleAfter time.Duration
	// Poll is the wait between checks.
	Poll time.Duration
}

// DrainResult is how a drain ended. Err carries a probe error that was still
// present at the timeout, and Drained is false whenever Err is set.
type DrainResult struct {
	Drained  bool
	Blocking []string
	Waited   time.Duration
	Err      error
}

// Drain polls the seats until none is working and recently active, or Timeout
// passes, and reports the seats that held it back, never acting on one and
// never reading an unreadable seat list as drained.
func Drain(ctx context.Context, clock Clock, sleep func(context.Context, time.Duration) error, p SeatProbe, o DrainOptions) (DrainResult, error) {
	start := clock.Now()
	var lastErr error
	var blocking []string
	for {
		if err := ctx.Err(); err != nil {
			return DrainResult{}, err
		}
		now := clock.Now()
		seats, err := p.Seats(ctx)
		if err != nil {
			// An unreadable seat list must never pass as an empty one, so this
			// poll cannot end the drain; the next poll retries the read.
			lastErr = err
		} else {
			lastErr = nil
			blocking = blockingSeats(seats, now, o.IdleAfter)
			if len(blocking) == 0 {
				return DrainResult{Drained: true, Waited: now.Sub(start)}, nil
			}
		}
		if now.Sub(start) >= o.Timeout {
			return DrainResult{Drained: false, Blocking: blocking, Waited: now.Sub(start), Err: lastErr}, nil
		}
		// The sleep seam waits out the interval and reports the context's error,
		// which ends the drain.
		if err := sleep(ctx, o.Poll); err != nil {
			return DrainResult{}, err
		}
	}
}

// blockingSeats names the seats that are working and whose last activity is
// younger than idleAfter. A working seat silent for idleAfter or longer is
// left alone, so a seat stuck at its prompt cannot hold an update up forever.
func blockingSeats(seats []Seat, now time.Time, idleAfter time.Duration) []string {
	var names []string
	for _, s := range seats {
		if s.Working && now.Sub(s.LastActive) < idleAfter {
			names = append(names, s.Name)
		}
	}
	return names
}
