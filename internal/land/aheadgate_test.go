package land

import (
	"context"
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
