package update

import (
	"context"
	"fmt"
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
	// Poll is the wait between checks. It must be positive.
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
// never reading an unreadable seat list as drained. Each probe call runs under
// the time left in Timeout, except the single first poll of a Timeout of zero
// or less, which runs unbounded.
func Drain(ctx context.Context, clock Clock, sleep func(context.Context, time.Duration) error, p SeatProbe, o DrainOptions) (DrainResult, error) {
	if o.Poll <= 0 {
		return DrainResult{}, fmt.Errorf("drain poll interval must be positive, got %v", o.Poll)
	}
	start := clock.Now()
	var lastErr error
	var blocking []string
	for polls := 0; ; polls++ {
		if err := ctx.Err(); err != nil {
			return DrainResult{}, err
		}
		now := clock.Now()
		elapsed := now.Sub(start)
		// Once Timeout has passed no further poll starts, so each later probe
		// call has budget left. The first poll always runs.
		if polls > 0 && elapsed >= o.Timeout {
			return DrainResult{Drained: false, Blocking: blocking, Waited: elapsed, Err: lastErr}, nil
		}
		seats, err := probeSeats(ctx, p, o.Timeout-elapsed)
		if err != nil {
			// An unreadable seat list must never pass as an empty one, so this
			// poll cannot end the drain; the next poll retries the read. The
			// seats named by an earlier poll are no longer known, so they are
			// not reported beside the error.
			lastErr = err
			blocking = nil
		} else {
			lastErr = nil
			blocking = blockingSeats(seats, now, o.IdleAfter)
			if len(blocking) == 0 {
				return DrainResult{Drained: true, Waited: elapsed}, nil
			}
		}
		// The sleep seam waits out the interval and reports the context's error,
		// which ends the drain.
		if err := sleep(ctx, o.Poll); err != nil {
			return DrainResult{}, err
		}
	}
}

// probeSeats reads the seats within the time left in Timeout. The read runs in
// its own goroutine, so a probe that ignores its context still cannot hold the
// drain past the budget: when the budget ends first, probeSeats returns the
// context's error, and the goroutine ends on its own once the probe returns.
// With no budget left, only the first poll of a Timeout of zero or less gets
// here, and that read is not bounded.
func probeSeats(ctx context.Context, p SeatProbe, remaining time.Duration) ([]Seat, error) {
	if remaining <= 0 {
		return p.Seats(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	type read struct {
		seats []Seat
		err   error
	}
	// The channel is buffered, so a probe that returns after the budget ends
	// does not block forever on the send.
	done := make(chan read, 1)
	go func() {
		seats, err := p.Seats(ctx)
		done <- read{seats, err}
	}()
	select {
	case r := <-done:
		return r.seats, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
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
