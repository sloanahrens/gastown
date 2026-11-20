package bdgateway

import (
	"context"
	"time"
)

// RunPeakLog writes one line per minute in which calls held a slot, so the cap
// can be tuned from data; it returns when ctx ends.
func RunPeakLog(ctx context.Context, ticks <-chan time.Time, take func() (peak, inFlight, capacity, refused uint64), logf func(format string, args ...any)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			// take runs on every tick, not only a busy one, so an idle minute
			// ends its window here and the next line reports only what came after.
			peak, inFlight, capacity, refused := take()
			if peak == 0 {
				continue
			}
			logf("slots: peak %d of %d in the last minute, %d in flight now, %d refused since start", peak, capacity, inFlight, refused)
		}
	}
}
