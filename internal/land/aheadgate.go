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
// is only stored.
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
// any of the three, or an empty slot, returns false and leaves the car held. If
// ctx ends first, Take returns false and the car stays held. Take holds no lock
// while it waits.
func (s *AheadSlot) Take(ctx context.Context, bead, head, base string) (AheadGate, bool) {
	if s == nil {
		return AheadGate{}, false
	}
	s.mu.Lock()
	car := s.car
	s.mu.Unlock()
	if car == nil || car.bead != bead || car.head != head || car.base != base {
		return AheadGate{}, false
	}
	select {
	case <-car.done:
	default:
		select {
		case <-car.done:
		case <-ctx.Done():
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
