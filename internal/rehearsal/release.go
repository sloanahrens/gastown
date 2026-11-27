package rehearsal

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Release starts sling for each id in order, with at most inFlight running at once,
// and returns one Sling per id in the same order. An inFlight below one runs one at
// a time, so it cannot deadlock. Seconds is the time now reports across each sling.
func Release(ctx context.Context, ids []string, inFlight int, sling func(ctx context.Context, id string) error, now func() time.Time) []Sling {
	inFlight = max(inFlight, 1)
	out := make([]Sling, len(ids))
	slots := make(chan struct{}, inFlight)
	var wg sync.WaitGroup
	for i, id := range ids {
		slots <- struct{}{} // taken before the goroutine starts, so starts follow id order
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			start := now()
			err := sling(ctx, id)
			out[i] = Sling{Bead: id, Outcome: slingOutcome(err), Seconds: now().Sub(start).Seconds()}
		}()
	}
	wg.Wait()
	return out
}

// slingOutcome is SlingOK for nil, SlingKilled for a deadline, and SlingFailed otherwise.
func slingOutcome(err error) string {
	if err == nil {
		return SlingOK
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return SlingKilled
	}
	return SlingFailed
}
