package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// errSeatDeadline reports that no seat came free before the deadline fired.
var errSeatDeadline = errors.New("rehearsal: no seat came free before the deadline")

// awaitSeat returns nil once free reports more seats than running reports slings
// in flight, at once or after a tick and a re-read, and wraps a failed read's
// error; a canceled ctx, fired deadline or closed tick ends it before a read or while waiting.
func awaitSeat(ctx context.Context, free func(context.Context) (int, error), running func() int, tick, deadline <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errSeatDeadline
		default:
		}
		if seats, err := free(ctx); err != nil {
			return fmt.Errorf("reading free seats: %w", err)
		} else if seats > running() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errSeatDeadline
		case _, ok := <-tick:
			if !ok {
				return errTickClosed
			}
		}
	}
}
