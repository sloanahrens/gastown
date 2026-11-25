package estimate

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
)

// TestJevInputFailedCalls: a failed call is left out of the median, and every
// call failing skips the Jev input with the first error as its reason.
func TestJevInputFailedCalls(t *testing.T) {
	t.Parallel()
	// Call 2 fails; the other two answer 0.43 and 0.49, so the median of what is
	// left is 0.46, where a failure counted as 0 would rest on 0.43.
	jev := &jevCounter{fits: func(call int) float64 { return []float64{0.43, -1, 0.49}[call-1] }}
	got, used, skip := jevInput(context.Background(), Deps{JevAsk: jev.ask}, sampleBead())
	if skip != nil || jev.calls != jevCalls || used != 2 || math.Abs(got-0.46) > 1e-9 {
		t.Fatalf("jevInput = %v from %d answers, skip %v after %d calls; want the median 0.46 from 2 answers", got, used, skip, jev.calls)
	}
	_, _, skip = jevInput(context.Background(), Deps{JevAsk: fakeJev(0, errors.New("battery is down"))}, sampleBead())
	if skip == nil || skip.Input != "jev" || skip.Reason != "battery is down" {
		t.Fatalf("jevInput skip = %+v, want the Jev input skipped with the first error", skip)
	}
}

// TestEstimateJevAsksRunTogether: every ask blocks until all jevCalls asks are in
// flight, so asking one after another leaves the first ask waiting for the others
// until the bubble's fake deadline ends it, which fails the test with no real wait. The
// median and the answer count are the same as when the asks ran in turn.
func TestEstimateJevAsksRunTogether(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		arrived := 0
		allInFlight := make(chan struct{})
		ask := func(ctx context.Context, _ *beads.Issue) (dispatch.Answers, error) {
			mu.Lock()
			arrived++
			if arrived == jevCalls {
				close(allInFlight)
			}
			mu.Unlock()
			select {
			case <-allInFlight:
				return dispatch.Answers{FitsTwentyMinutes: 0.4}, nil
			case <-ctx.Done():
				return dispatch.Answers{}, ctx.Err()
			}
		}
		// The bubble's clock is fake: when every goroutine waits, the deadline
		// passes at once, so asks run in turn fail here instead of hanging.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		got, used, skip := jevInput(ctx, Deps{JevAsk: ask}, sampleBead())
		if skip != nil || used != jevCalls || math.Abs(got-0.4) > 1e-9 {
			t.Fatalf("jevInput = %v, %d answers, skip %v; want the median 0.4 from %d answers", got, used, skip, jevCalls)
		}
	})
}
