package land

import (
	"context"
	"sync"
)

// AheadGate is one candidate the landing worker gated ahead of its turn: the
// bead, head and base it was built on, the merged commit, and the gate's
// verdict. It is held in memory only (gt-6ivh2m).
type AheadGate struct {
	Bead, Head, Base, Merged string
	Result                   CandidateResult
}

// AheadSlot holds at most one car gated ahead of its turn. The zero value is
// ready, and the slot is safe for use from two goroutines. A nil *AheadSlot is
// the switch being off: nothing starts.
type AheadSlot struct {
	mu  sync.Mutex
	car *aheadCar
}

// aheadCar is the car a slot holds. The run goroutine writes merged, gate and
// err under the slot's mu before it closes done.
type aheadCar struct {
	bead, head, base string
	discard          func(AheadGate)
	cancel           context.CancelFunc
	done             chan struct{}
	merged           string
	gate             AheadGate
	err              error
}

// Start runs run on its own goroutine, with a context that ends when ctx does,
// and keeps what run returns with the car (gt-o1w1rv). The gate it keeps names
// the bead, head and base Start was given, and the merged commit report
// recorded when run's own result names none (gt-6ivh2m). It starts nothing and
// returns false on a nil slot, with a nil run, or while a car is held. discard
// is called at most once, by a Take or Close that abandons a car whose run
// reported a merged commit; a nil discard discards nothing.
func (s *AheadSlot) Start(ctx context.Context, bead, head, base string, run func(ctx context.Context, report func(merged string)) (AheadGate, error), discard func(AheadGate)) bool {
	if s == nil || run == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.car != nil {
		return false
	}
	runCtx, cancel := context.WithCancel(ctx)
	car := &aheadCar{bead: bead, head: head, base: base, discard: discard, cancel: cancel, done: make(chan struct{})}
	s.car = car
	go func() {
		defer close(car.done)
		defer cancel()
		gate, err := run(runCtx, func(merged string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			car.merged = merged
		})
		s.mu.Lock()
		defer s.mu.Unlock()
		// The key comes from the car, so Take matches what Start was given.
		gate.Bead, gate.Head, gate.Base = car.bead, car.head, car.base
		if gate.Merged == "" {
			gate.Merged = car.merged
		}
		car.gate, car.err = gate, err
	}()
	return true
}

// Take waits for the held car to finish, then empties the slot and returns the
// gate and true when the car matches bead, head and base and its run returned no
// error. A failed run empties the slot and returns false. A key that differs in
// any of the three abandons the car: Take cancels its run, waits for it to end,
// discards it when its run reported a merged commit, empties the slot and
// returns false. A Take whose own ctx ends before the run does abandons the car
// the same way. An empty slot returns false. Take holds no lock while it waits.
func (s *AheadSlot) Take(ctx context.Context, bead, head, base string) (AheadGate, bool) {
	if s == nil {
		return AheadGate{}, false
	}
	s.mu.Lock()
	car := s.car
	s.mu.Unlock()
	if car == nil {
		return AheadGate{}, false
	}
	if car.bead != bead || car.head != head || car.base != base {
		s.abandon(car)
		return AheadGate{}, false
	}
	select {
	case <-car.done:
	default:
		select {
		case <-car.done:
		case <-ctx.Done():
			s.abandon(car)
			return AheadGate{}, false
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.car != car {
		return AheadGate{}, false
	}
	s.car = nil
	if car.err != nil {
		return AheadGate{}, false
	}
	return car.gate, true
}

// Close cancels the held car's run, waits for it to end, discards it when its
// run reported a merged commit, and empties the slot. A nil or empty slot does
// nothing.
func (s *AheadSlot) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	car := s.car
	s.mu.Unlock()
	if car != nil {
		s.abandon(car)
	}
}

// abandon cancels car's run and waits for it to end, then empties the slot and
// discards the car once when its run reported a merged commit. A car another
// Take or Close already emptied is left alone, so a taken car is never
// discarded and a car is discarded at most once.
func (s *AheadSlot) abandon(car *aheadCar) {
	car.cancel()
	<-car.done
	s.mu.Lock()
	if s.car != car {
		s.mu.Unlock()
		return
	}
	s.car = nil
	gate, discard := car.gate, car.discard
	s.mu.Unlock()
	if gate.Merged == "" || discard == nil {
		return
	}
	discard(gate)
}

// Pending reports the key of the held car and whether one is held, also after
// its run returned. A nil or empty slot holds nothing.
func (s *AheadSlot) Pending() (bead, head, base string, ok bool) {
	if s == nil {
		return "", "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.car == nil {
		return "", "", "", false
	}
	return s.car.bead, s.car.head, s.car.base, true
}
