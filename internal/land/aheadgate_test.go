package land

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
)

func TestAheadSlotStartsOneRunAndRefusesWhileHeld(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		started := make(chan context.Context, 2)
		release := make(chan struct{})
		run := func(ctx context.Context, report func(string)) (AheadGate, error) {
			started <- ctx
			report("m1") // run may always report, held car or not
			select {
			case <-release:
			case <-ctx.Done():
			}
			return AheadGate{}, ctx.Err()
		}
		refused := func(context.Context, func(string)) (AheadGate, error) {
			t.Error("a refused Start ran its run")
			return AheadGate{}, nil
		}
		var s AheadSlot
		if !s.Start(parent, "gt-a", "h1", "main", run, nil) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		if s.Start(parent, "gt-b", "h2", "main", refused, nil) {
			t.Error("second Start during the first run = true, want false")
		}
		synctest.Wait()
		var runCtx context.Context
		select {
		case runCtx = <-started:
		default:
			t.Fatal("run did not start")
		}
		if n := len(started); n != 0 {
			t.Fatalf("run started %d more times, want once", n)
		}
		if err := runCtx.Err(); err != nil {
			t.Fatalf("run's ctx ended before the parent did: %v", err)
		}
		cancel()
		synctest.Wait()
		if runCtx.Err() == nil {
			t.Error("run's ctx is live after the parent ended")
		}
		if s.Start(context.Background(), "gt-b", "h2", "main", refused, nil) {
			t.Error("second Start after the first run returned = true, want false")
		}
		synctest.Wait()
	})
}

func TestAheadSlotOffStartsNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ran := make(chan struct{}, 2)
		run := func(context.Context, func(string)) (AheadGate, error) {
			ran <- struct{}{}
			return AheadGate{}, nil
		}
		var off *AheadSlot
		if off.Start(t.Context(), "gt-a", "h1", "main", run, nil) {
			t.Error("Start on a nil slot = true, want false")
		}
		var s AheadSlot
		if s.Start(t.Context(), "gt-a", "h1", "main", nil, nil) {
			t.Error("Start with a nil run = true, want false")
		}
		synctest.Wait()
		if len(ran) != 0 {
			t.Errorf("a refused Start ran %d times", len(ran))
		}
		if !s.Start(t.Context(), "gt-a", "h1", "main", run, nil) {
			t.Error("Start after a nil run = false, want true: a refusal holds nothing")
		}
		synctest.Wait()
	})
}

func TestAheadSlotPendingNamesTheHeldCar(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var off *AheadSlot
		if _, _, _, ok := off.Pending(); ok {
			t.Error("Pending on a nil slot = held, want empty")
		}
		var s AheadSlot
		if _, _, _, ok := s.Pending(); ok {
			t.Error("Pending on an empty slot = held, want empty")
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		returned := make(chan struct{})
		release := make(chan struct{})
		run := func(ctx context.Context, report func(string)) (AheadGate, error) {
			defer close(returned)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return AheadGate{}, ctx.Err()
		}
		if !s.Start(ctx, "gt-a", "h1", "main", run, nil) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		synctest.Wait()
		if bead, head, base, ok := s.Pending(); !ok || bead != "gt-a" || head != "h1" || base != "main" {
			t.Fatalf("Pending while the run is live = (%q, %q, %q, %v), want the held car", bead, head, base, ok)
		}
		cancel()
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Fatal("run did not return after its ctx ended")
		}
		if bead, head, base, ok := s.Pending(); !ok || bead != "gt-a" || head != "h1" || base != "main" {
			t.Errorf("Pending after the run returned = (%q, %q, %q, %v), want the car still held", bead, head, base, ok)
		}
	})
}

// finished is a run that returns gate and err at once.
func finished(gate AheadGate, err error) func(context.Context, func(string)) (AheadGate, error) {
	return func(context.Context, func(string)) (AheadGate, error) { return gate, err }
}

// blocked is a run that reports merged when it is non-empty, then returns
// ctx.Err once ctx ends or release closes, closing returned as it does. A nil
// release never fires.
func blocked(returned chan struct{}, release <-chan struct{}, merged string) func(context.Context, func(string)) (AheadGate, error) {
	return func(ctx context.Context, report func(string)) (AheadGate, error) {
		defer close(returned)
		if merged != "" {
			report(merged)
		}
		select {
		case <-ctx.Done():
		case <-release:
		}
		return AheadGate{}, ctx.Err()
	}
}

// discards records the gates a slot discarded, safe from any goroutine.
type discards struct {
	mu sync.Mutex
	gs []AheadGate
}

func (d *discards) add(g AheadGate) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gs = append(d.gs, g)
}

func (d *discards) all() []AheadGate {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]AheadGate(nil), d.gs...)
}

// TestAheadSlotTakeWaitsForTheRunAndReturnsItOnce shows a matching Take blocked
// until the held run returns, then handed its gate once with the slot empty and
// nothing discarded.
func TestAheadSlotTakeWaitsForTheRunAndReturnsItOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		var discarded discards
		want := AheadGate{Bead: "gt-a", Head: "h1", Base: "main", Result: CandidateResult{State: CandidatePassed}}
		var s AheadSlot
		if !s.Start(ctx, "gt-a", "h1", "main", func(ctx context.Context, _ func(string)) (AheadGate, error) {
			select {
			case <-release:
				return want, nil
			case <-ctx.Done():
				return AheadGate{}, ctx.Err()
			}
		}, discarded.add) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		gates := make(chan AheadGate, 1)
		oks := make(chan bool, 1)
		go func() {
			g, ok := s.Take(ctx, "gt-a", "h1", "main")
			gates <- g
			oks <- ok
		}()
		synctest.Wait()
		select {
		case <-gates:
			t.Fatal("Take returned before the run ended")
		default:
		}
		close(release)
		synctest.Wait()
		select {
		case gate := <-gates:
			if ok := <-oks; !ok || gate != want {
				t.Fatalf("Take = (%+v, %v), want (%+v, true)", gate, ok, want)
			}
		default:
			t.Fatal("Take did not return after the run ended")
		}
		if n := len(discarded.all()); n != 0 {
			t.Errorf("discard calls for a taken car = %d, want 0", n)
		}
		if _, ok := s.Take(ctx, "gt-a", "h1", "main"); ok {
			t.Error("second Take = true, want false: a result is taken once")
		}
		if _, _, _, ok := s.Pending(); ok {
			t.Error("Pending after Take = held, want empty")
		}
	})
}

// TestAheadSlotTakeAbandonsACarThatDiffers shows a Take differing in bead, head
// or base canceling the held run, waiting for it to end, discarding a merged
// candidate once and emptying the slot, and discarding nothing without one.
func TestAheadSlotTakeAbandonsACarThatDiffers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, bead, head, base, merged string
		discards                       int
	}{
		{"differs in bead", "gt-x", "h1", "main", "m1", 1},
		{"differs in head", "gt-a", "h9", "main", "m1", 1},
		{"differs in base", "gt-a", "h1", "other", "m1", 1},
		{"reported no merged commit", "gt-x", "h1", "main", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				returned := make(chan struct{})
				var discarded discards
				var s AheadSlot
				if !s.Start(ctx, "gt-a", "h1", "main", blocked(returned, nil, tc.merged), discarded.add) {
					t.Fatal("Start on an empty slot = false, want true")
				}
				synctest.Wait()
				if _, ok := s.Take(ctx, tc.bead, tc.head, tc.base); ok {
					t.Fatal("Take with a differing key = true, want false")
				}
				select {
				case <-returned:
				default:
					t.Fatal("run outlived the differing Take")
				}
				got := discarded.all()
				if len(got) != tc.discards {
					t.Fatalf("discard calls after a differing Take = %d, want %d", len(got), tc.discards)
				}
				if want := (AheadGate{Bead: "gt-a", Head: "h1", Base: "main", Merged: "m1"}); tc.discards == 1 && got[0] != want {
					t.Errorf("discarded = %+v, want %+v", got, want)
				}
				if _, _, _, ok := s.Pending(); ok {
					t.Error("Pending after a differing Take = held, want empty")
				}
				synctest.Wait()
			})
		})
	}
}

// TestAheadSlotTakeWithNilDiscardAbandonsACar shows a differing Take emptying
// the slot of a reported car that has no discard hook.
func TestAheadSlotTakeWithNilDiscardAbandonsACar(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var s AheadSlot
		if !s.Start(context.Background(), "gt-a", "h1", "main", finished(AheadGate{Merged: "m-run"}, nil), nil) {
			t.Fatal("Start with a nil discard = false, want true")
		}
		synctest.Wait()
		if _, ok := s.Take(context.Background(), "gt-x", "h1", "main"); ok {
			t.Fatal("Take with a differing key = true, want false")
		}
		if _, _, _, ok := s.Pending(); ok {
			t.Error("Pending after a differing Take = held, want empty")
		}
		synctest.Wait()
	})
}

// TestAheadSlotTakeEmptiesAFailedRun shows a failed run's Take returning false
// with the slot emptied, and a nil or empty slot handing back nothing.
func TestAheadSlotTakeEmptiesAFailedRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var off *AheadSlot
		if _, ok := off.Take(ctx, "gt-a", "h1", "main"); ok {
			t.Error("Take on a nil slot = true, want false")
		}
		var empty AheadSlot
		if _, ok := empty.Take(ctx, "gt-a", "h1", "main"); ok {
			t.Error("Take on an empty slot = true, want false")
		}
		var s AheadSlot
		if !s.Start(ctx, "gt-a", "h1", "main", finished(AheadGate{}, errors.New("gate: push refused")), nil) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		synctest.Wait()
		if _, ok := s.Take(ctx, "gt-a", "h1", "main"); ok {
			t.Fatal("Take of a failed run = true, want false")
		}
		if _, _, _, ok := s.Pending(); ok {
			t.Fatal("Pending after a failed run was taken = held, want empty")
		}
	})
}

// TestAheadSlotTakeWithEndedCtxAbandonsTheCar shows a Take whose ctx ends
// before the held run does canceling it, waiting for it to end, discarding its
// merged candidate once and emptying the slot.
func TestAheadSlotTakeWithEndedCtxAbandonsTheCar(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())
		defer cancelParent()
		returned := make(chan struct{})
		var discarded discards
		var s AheadSlot
		if !s.Start(parent, "gt-a", "h1", "main", blocked(returned, nil, "m1"), discarded.add) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		takeCtx, cancelTake := context.WithCancel(context.Background())
		defer cancelTake()
		oks := make(chan bool, 1)
		go func() {
			_, ok := s.Take(takeCtx, "gt-a", "h1", "main")
			oks <- ok
		}()
		synctest.Wait()
		cancelTake()
		synctest.Wait()
		select {
		case ok := <-oks:
			if ok {
				t.Fatal("Take with a canceled ctx = true, want false")
			}
		default:
			t.Fatal("Take did not return after its ctx ended")
		}
		select {
		case <-returned:
		default:
			t.Fatal("run outlived the canceled Take")
		}
		if got := discarded.all(); len(got) != 1 || got[0] != (AheadGate{Bead: "gt-a", Head: "h1", Base: "main", Merged: "m1"}) {
			t.Errorf("discarded = %+v, want one merged candidate", got)
		}
		if _, _, _, ok := s.Pending(); ok {
			t.Error("Pending after a canceled Take = held, want empty")
		}
		synctest.Wait()
	})
}

// TestAheadSlotCloseAbandonsTheHeldCar shows Close canceling the held run,
// waiting for it to end, discarding its merged candidate once and emptying the
// slot, and doing nothing on a nil or empty slot.
func TestAheadSlotCloseAbandonsTheHeldCar(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var off *AheadSlot
		off.Close()
		var empty AheadSlot
		empty.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		returned := make(chan struct{})
		var discarded discards
		var s AheadSlot
		if !s.Start(ctx, "gt-a", "h1", "main", blocked(returned, nil, "m1"), discarded.add) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		synctest.Wait()
		s.Close()
		select {
		case <-returned:
		default:
			t.Fatal("run outlived Close")
		}
		if got := discarded.all(); len(got) != 1 || got[0] != (AheadGate{Bead: "gt-a", Head: "h1", Base: "main", Merged: "m1"}) {
			t.Errorf("discarded = %+v, want one merged candidate", got)
		}
		if _, _, _, ok := s.Pending(); ok {
			t.Error("Pending after Close = held, want empty")
		}
		s.Close()
		if n := len(discarded.all()); n != 1 {
			t.Errorf("discard calls after a second Close = %d, want still 1", n)
		}
		synctest.Wait()
	})
}

// TestAheadSlotTwoAbandonsDiscardOnce shows a Close and a differing Take both
// holding the same car: one empties and discards it once, the other finds it
// gone. The run waits on release, so both callers snatch the car before it ends.
func TestAheadSlotTwoAbandonsDiscardOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		returned := make(chan struct{})
		var discarded discards
		var s AheadSlot
		if !s.Start(context.Background(), "gt-a", "h1", "main", func(ctx context.Context, report func(string)) (AheadGate, error) {
			defer close(returned)
			report("m1")
			select {
			case <-release:
			case <-ctx.Done():
			}
			return AheadGate{}, errors.New("car: run released")
		}, discarded.add) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		synctest.Wait()
		settled := make(chan struct{}, 2)
		go func() {
			s.Close()
			settled <- struct{}{}
		}()
		go func() {
			s.Take(context.Background(), "gt-x", "h1", "main")
			settled <- struct{}{}
		}()
		synctest.Wait()
		close(release)
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Fatal("run did not return after release")
		}
		for i := 0; i < 2; i++ {
			select {
			case <-settled:
			default:
				t.Fatal("an abandon did not return")
			}
		}
		if got := discarded.all(); len(got) != 1 || got[0] != (AheadGate{Bead: "gt-a", Head: "h1", Base: "main", Merged: "m1"}) {
			t.Errorf("discarded = %+v, want one merged candidate", got)
		}
		if _, _, _, ok := s.Pending(); ok {
			t.Error("Pending after both abandons = held, want empty")
		}
		synctest.Wait()
	})
}

// TestAheadSlotTwoTakesOneGetsTheCar shows two Takes blocked on one held car
// with exactly one returning the gate and the slot empty: the Take that finds
// the car no longer held returns false.
func TestAheadSlotTwoTakesOneGetsTheCar(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		want := AheadGate{Bead: "gt-a", Head: "h1", Base: "main", Result: CandidateResult{State: CandidatePassed}}
		var s AheadSlot
		if !s.Start(ctx, "gt-a", "h1", "main", func(ctx context.Context, _ func(string)) (AheadGate, error) {
			select {
			case <-release:
				return want, nil
			case <-ctx.Done():
				return AheadGate{}, ctx.Err()
			}
		}, nil) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		type taken struct {
			gate AheadGate
			ok   bool
		}
		results := make(chan taken, 2)
		take := func() {
			gate, ok := s.Take(ctx, "gt-a", "h1", "main")
			results <- taken{gate, ok}
		}
		go take()
		go take()
		synctest.Wait()
		if len(results) != 0 {
			t.Fatal("a Take returned while the car was still held, want both blocked")
		}
		close(release)
		synctest.Wait()
		wins := 0
		collect := func() {
			select {
			case r := <-results:
				if r.ok && r.gate == want {
					wins++
				}
			default:
				t.Fatal("a Take has no result after the run ended")
			}
		}
		collect()
		collect()
		if wins != 1 {
			t.Errorf("Takes that got the result = %d, want exactly 1", wins)
		}
		if _, _, _, ok := s.Pending(); ok {
			t.Error("Pending after the Takes = held, want empty")
		}
	})
}

// TestAheadSlotTakeWithEndedCtxReturnsAFinishedRun shows a Take whose ctx has
// already ended still returning the result of a run that has already ended.
func TestAheadSlotTakeWithEndedCtxReturnsAFinishedRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		want := AheadGate{Bead: "gt-a", Head: "h1", Base: "main", Result: CandidateResult{State: CandidatePassed}}
		var s AheadSlot
		if !s.Start(context.Background(), "gt-a", "h1", "main", finished(want, nil), nil) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		synctest.Wait()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if gate, ok := s.Take(ctx, "gt-a", "h1", "main"); !ok || gate != want {
			t.Fatalf("Take with an ended ctx = (%+v, %v), want (%+v, true)", gate, ok, want)
		}
	})
}

// TestAheadSlotTakeNamesTheCarItWasBuiltFor shows a taken result carrying the
// key Start was given though run set others, and the reported merged commit.
func TestAheadSlotTakeNamesTheCarItWasBuiltFor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var s AheadSlot
		if !s.Start(ctx, "gt-a", "h1", "main", func(_ context.Context, report func(string)) (AheadGate, error) {
			report("m-reported")
			return AheadGate{Bead: "gt-x", Head: "hx", Base: "bx"}, nil
		}, nil) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		synctest.Wait()
		gate, ok := s.Take(ctx, "gt-a", "h1", "main")
		if want := (AheadGate{Bead: "gt-a", Head: "h1", Base: "main", Merged: "m-reported"}); !ok || gate != want {
			t.Fatalf("Take = (%+v, %v), want (%+v, true)", gate, ok, want)
		}
	})
}

// TestAheadSlotTakeKeepsTheMergedCommitRunNamed shows run's merged commit kept
// over the one report recorded.
func TestAheadSlotTakeKeepsTheMergedCommitRunNamed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var s AheadSlot
		if !s.Start(ctx, "gt-a", "h1", "main", func(_ context.Context, report func(string)) (AheadGate, error) {
			report("m-reported")
			return AheadGate{Merged: "m-run"}, nil
		}, nil) {
			t.Fatal("Start on an empty slot = false, want true")
		}
		synctest.Wait()
		if gate, ok := s.Take(ctx, "gt-a", "h1", "main"); !ok || gate.Merged != "m-run" {
			t.Fatalf("Take Merged = (%q, %v), want (%q, true)", gate.Merged, ok, "m-run")
		}
	})
}
