package rehearsal

import (
	"context"
	"errors"
	"time"
)

// errTickClosed reports that the tick channel closed before the run window ended.
var errTickClosed = errors.New("rehearsal: the run window tick channel closed")

// Wait polls each open id once per tick and returns the ids still open when the
// window ends: none if all are done, the open ids if the deadline fires, the open
// ids with ctx's error if ctx is canceled, and the open ids with errTickClosed
// if the tick closes. A failed read keeps its id open; three ticks in a row that
// fail every read return the last error. A canceled ctx or a fired deadline
// wins over a ready tick.
func Wait(ctx context.Context, ids []string, state func(context.Context, string) (bool, error), tick <-chan time.Time, deadline <-chan time.Time) ([]string, error) {
	open := append([]string(nil), ids...)
	var lastErr error
	failed := 0
	for len(open) > 0 {
		if err := ctx.Err(); err != nil {
			return open, err
		}
		// Check the deadline before the select so a window that has ended stops
		// even when a tick is also ready, rather than racing it.
		select {
		case <-deadline:
			return open, nil
		default:
		}
		select {
		case <-deadline:
			return open, nil
		case <-ctx.Done():
			return open, ctx.Err()
		case _, ok := <-tick:
			if !ok {
				return open, errTickClosed
			}
		}
		still := open[:0]
		readOK := false
		for _, id := range open {
			done, err := state(ctx, id)
			if err != nil {
				lastErr = err
				still = append(still, id)
				continue
			}
			readOK = true
			if !done {
				still = append(still, id)
			}
		}
		open = still
		if readOK {
			failed = 0
			continue
		}
		if failed++; failed == 3 {
			return open, lastErr
		}
	}
	return nil, nil
}
