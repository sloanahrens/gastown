package rehearsal

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Release starts sling for each id in order, with at most inFlight running at once,
// and returns one Sling per id in the same order. An inFlight below one runs one at
// a time, so it cannot deadlock. Before each sling starts admit gets a function
// reporting how many of this Release's slings are in flight; nil admits at once and
// its time is not in Seconds. An admit error, or a cancel during it, starts no
// further sling and reads that id and the rest failed in zero seconds; started
// slings keep their outcome. Once ctx is canceled no further sling starts, and the
// ids it never reached read as failed in zero seconds. Seconds is the time now
// reports across each sling.
func Release(ctx context.Context, ids []string, inFlight int, sling func(ctx context.Context, id string) error, admit func(ctx context.Context, running func() int) error, now func() time.Time) []Sling {
	inFlight = max(inFlight, 1)
	out := make([]Sling, len(ids))
	slots := make(chan struct{}, inFlight)
	var running atomic.Int32
	var wg sync.WaitGroup
	for i, id := range ids {
		select {
		case slots <- struct{}{}: // taken before the goroutine starts, so starts follow id order
		case <-ctx.Done():
			out[i] = Sling{Bead: id, Outcome: SlingFailed}
			continue
		}
		if ctx.Err() != nil { // both cases were ready and the slot won: start nothing
			<-slots
			out[i] = Sling{Bead: id, Outcome: SlingFailed}
			continue
		}
		if admit != nil {
			err := admit(ctx, func() int { return int(running.Load()) })
			if err != nil || ctx.Err() != nil { // no further sling starts, and admit is not called again
				<-slots
				for j := i; j < len(ids); j++ {
					out[j] = Sling{Bead: ids[j], Outcome: SlingFailed}
				}
				break
			}
		}
		running.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer running.Add(-1) // before the slot frees, so admit never counts a finished sling
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
