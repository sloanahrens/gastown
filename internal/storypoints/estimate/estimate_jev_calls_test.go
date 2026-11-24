package estimate

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
)

// TestEstimateJevMedianSpread: each estimate gets two 13.565-minute answers
// (0.4) and one 11.645-minute answer (0.46) in a new order; the median pins
// the Jev input to 13.565 and holds the blended minutes within 0.3 of each other.
func TestEstimateJevMedianSpread(t *testing.T) {
	t.Parallel()
	jev := &jevCounter{fits: func(call int) float64 { return []float64{0.4, 0.4, 0.46, 0.4, 0.46, 0.4, 0.46, 0.4, 0.4}[call-1] }}
	deps := workingDeps()
	deps.JevAsk = jev.ask
	var minutes []float64
	for i := range 3 {
		res, err := Estimate(context.Background(), deps, sampleBead())
		if err != nil || res.Rung != "all" || math.Abs(*res.ComponentMinutes.Jev-13.565) > 1e-9 {
			t.Fatalf("estimate %d: rung %q skipped %+v err %v, want every input and the median's 13.565", i+1, res.Rung, res.Skipped, err)
		}
		minutes = append(minutes, res.Minutes)
	}
	if spread := slices.Max(minutes) - slices.Min(minutes); jev.calls != 9 || spread >= 0.3 {
		t.Fatalf("Jev calls = %d; blended minutes %v differ by %v, want 9 calls and under 0.3", jev.calls, minutes, spread)
	}
}

// TestEstimateJevFailedCalls: a failed call is left out of the median, and every
// call failing skips the Jev input with the first error as its reason.
func TestEstimateJevFailedCalls(t *testing.T) {
	t.Parallel()
	// Call 2 fails; the other two answer 0.43 and 0.49, so the median of what
	// is left is 0.46 (11.645 minutes), where a failure counted as 0 rests on 0.43.
	jev := &jevCounter{fits: func(call int) float64 { return []float64{0.43, -1, 0.49}[call-1] }}
	deps := workingDeps()
	deps.JevAsk = jev.ask
	got, err := Estimate(context.Background(), deps, sampleBead())
	if err != nil || jev.calls != jevCalls || got.Rung != "all" || math.Abs(*got.ComponentMinutes.Jev-11.645) > 1e-9 {
		t.Fatalf("rung %q skipped %+v after %d calls, err %v; want the median 11.645 from two answers", got.Rung, got.Skipped, jev.calls, err)
	}
	deps.JevAsk = fakeJev(0, errors.New("battery is down"))
	got, err = Estimate(context.Background(), deps, sampleBead())
	if err != nil || got.Rung != "shape-embed" || len(got.Skipped) != 1 || got.Skipped[0].Input != "jev" || got.Skipped[0].Reason != "battery is down" {
		t.Fatalf("rung %q skipped %+v err %v, want the Jev input skipped with the first error", got.Rung, got.Skipped, err)
	}
}

// TestEstimateJevAsksRunTogether: every ask blocks until all jevCalls asks are in
// flight, so asking one after another leaves the first ask waiting for the others
// and the bubble reports a deadlock, which fails the test without a clock. The
// median and the answer count are the same as when the asks ran in turn.
func TestEstimateJevAsksRunTogether(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		arrived := 0
		allInFlight := make(chan struct{})
		ask := func(context.Context, *beads.Issue) (dispatch.Answers, error) {
			mu.Lock()
			arrived++
			if arrived == jevCalls {
				close(allInFlight)
			}
			mu.Unlock()
			<-allInFlight
			return dispatch.Answers{FitsTwentyMinutes: 0.4}, nil
		}
		got, used, skip := jevInput(context.Background(), Deps{JevAsk: ask}, sampleBead())
		if skip != nil || used != jevCalls || math.Abs(got-0.4) > 1e-9 {
			t.Fatalf("jevInput = %v, %d answers, skip %v; want the median 0.4 from %d answers", got, used, skip, jevCalls)
		}
	})
}
