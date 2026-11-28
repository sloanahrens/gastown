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

// aheadCar is the car a slot holds. The run goroutine writes gate and err under
// the slot's mu before it closes done.
type aheadCar struct {
	bead, head, base string
	discard          func(AheadGate)
	cancel           context.CancelFunc
	done             chan struct{}
	gate             AheadGate
	err              error
}

// Start runs run on its own goroutine, with a context that ends when ctx does,
// and keeps what run returns with the car (gt-o1w1rv). It starts nothing and
// returns false on a nil slot, with a nil run, or while a car is held. The
// report given to run does nothing yet, and discard is only stored.
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
		gate, err := run(runCtx, func(string) {})
		s.mu.Lock()
		defer s.mu.Unlock()
		car.gate, car.err = gate, err
	}()
	return true
}
