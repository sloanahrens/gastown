package cmd

import (
	"context"
	"iter"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
)

// seatPickRigFake is one rig with idle seats for two concurrent pool picks. A
// claim removes its seat from every later IdlePolecats answer, and the fake
// accepts a second claim of a seat, as a reuse gate blind to a claim not yet
// hooked does.
//
// Each pick reports an arrival at its first step: LockSeatPick when the lock is
// taken, IdlePolecats when it is not. The test opens release once it has seen
// two events, so a read waits for the other pick to arrive without waiting for it
// to read: a pick blocked behind the lock cannot read until the other unlocks.
type seatPickRigFake struct {
	ctx context.Context // ends a wait for release when the test ends

	mu        sync.Mutex
	unclaimed []*polecat.Polecat

	seatLock sync.Mutex
	arrive   chan struct{}
	release  chan struct{}
}

func (f *seatPickRigFake) LockSeatPick() (func(), error) {
	f.arrive <- struct{}{}
	f.seatLock.Lock()
	return f.seatLock.Unlock, nil
}

// IdlePolecats snapshots the unclaimed seats before it arrives, so the snapshot
// a pick reads is fixed by the claims made before the read.
func (f *seatPickRigFake) IdlePolecats() (iter.Seq[*polecat.Polecat], error) {
	f.mu.Lock()
	seats := slices.Clone(f.unclaimed)
	f.mu.Unlock()
	f.arrive <- struct{}{}
	select {
	case <-f.release:
	case <-f.ctx.Done():
	}
	return slices.Values(seats), nil
}

func (f *seatPickRigFake) ReuseIdlePolecat(name string, _ polecat.AddOptions) (*polecat.Polecat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unclaimed = slices.DeleteFunc(f.unclaimed, func(p *polecat.Polecat) bool { return p.Name == name })
	return &polecat.Polecat{Name: name}, nil
}

func (f *seatPickRigFake) Get(name string) (*polecat.Polecat, error) {
	return &polecat.Polecat{Name: name}, nil
}

// TestSeatPickTwoSlingsGetDifferentSeats guards gt-0igsxt: two pool picks on one
// rig must not take the same seat. Both picks read the same two idle seats before
// either claims one, so without the seat-pick lock both reuse alpha. With the
// lock, the second pick reads only after the first has claimed and unlocked, and
// takes beta.
func TestSeatPickTwoSlingsGetDifferentSeats(t *testing.T) {
	t.Parallel()
	// A hang guard, not a timing assertion: every wait below resolves on a
	// channel, so a passing run never reaches this deadline. A regression that
	// blocks a pick fails here instead of stalling until go test's timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	fake := &seatPickRigFake{
		ctx:       ctx,
		unclaimed: []*polecat.Polecat{{Name: "alpha"}, {Name: "beta"}},
		arrive:    make(chan struct{}, 4),
		release:   make(chan struct{}),
	}

	type pick struct {
		info *SpawnedPolecatInfo
		err  error
	}
	picks := make(chan pick, 2)
	for i := 0; i < 2; i++ {
		go func() {
			info, err := reuseIdlePolecatForSlingWith(fake, fakeIdleReuseEnv(), "rig",
				SlingSpawnOptions{HookBead: "gt-next"}, func() polecat.RespawnMark { return polecat.RespawnMark{} })
			picks <- pick{info, err}
		}()
	}

	// Release both picks once two events have come in: an arrival, or a pick that
	// finished without one. Then collect both results.
	var results []pick
	events := 0
	for events < 2 {
		select {
		case <-fake.arrive:
			events++
		case p := <-picks:
			results = append(results, p)
			events++
		case <-ctx.Done():
			t.Fatal("the pool picks neither reached the seat read nor finished")
		}
	}
	close(fake.release)
	for len(results) < 2 {
		select {
		case p := <-picks:
			results = append(results, p)
		case <-ctx.Done():
			t.Fatal("a pool pick did not finish after release")
		}
	}

	for _, p := range results {
		if p.err != nil || p.info == nil {
			t.Fatalf("pool pick gave (%+v, %v); want a reused seat", p.info, p.err)
		}
	}
	if first, second := results[0].info.PolecatName, results[1].info.PolecatName; first == second {
		t.Errorf("both pool picks took seat %s; the seat-pick lock must keep the second pick from reading it", first)
	}
}
