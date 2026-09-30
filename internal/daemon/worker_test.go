package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRigWorkerPoolConcurrencyLimit verifies that the pool never runs more than
// the configured number of rigs simultaneously: the first maxWorkers rigs hold
// their slots until all of them are in, and no rig beyond them may start.
func TestRigWorkerPoolConcurrencyLimit(t *testing.T) {
	t.Parallel()
	const (
		numRigs    = 20
		maxWorkers = 5
	)

	pool := newRigWorkerPool(maxWorkers, 10*time.Second, nil)

	var active, peak, ran atomic.Int64
	full := make(chan struct{})
	var fullOnce sync.Once
	release := make(chan struct{})

	rigs := make([]string, numRigs)
	for i := range rigs {
		rigs[i] = "rig"
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.runPerRig(context.Background(), rigs, func(ctx context.Context, rigName string) error {
			cur := active.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			if cur == maxWorkers {
				fullOnce.Do(func() { close(full) })
			}
			<-release // hold the slot until the pool is full
			active.Add(-1)
			ran.Add(1)
			return nil
		})
	}()

	<-full
	close(release)
	<-done

	if got := peak.Load(); got != maxWorkers {
		t.Errorf("peak concurrency %d, want exactly the limit %d", got, maxWorkers)
	}
	if got := ran.Load(); got != numRigs {
		t.Errorf("ran %d rigs, want %d", got, numRigs)
	}
}

// TestRigWorkerPoolContextTimeout verifies that per-rig context timeouts fire:
// rigs that never finish are each cancelled, and runPerRig returns.
func TestRigWorkerPoolContextTimeout(t *testing.T) {
	t.Parallel()
	const numRigs = 5

	pool := newRigWorkerPool(numRigs, 50*time.Millisecond, nil)

	var cancelled atomic.Int64
	rigs := make([]string, numRigs)
	for i := range rigs {
		rigs[i] = "rig"
	}

	pool.runPerRig(context.Background(), rigs, func(ctx context.Context, _ string) error {
		<-ctx.Done() // never finishes on its own
		cancelled.Add(1)
		return ctx.Err()
	})

	if got := cancelled.Load(); got != numRigs {
		t.Errorf("cancelled %d rigs, want all %d by their timeout", got, numRigs)
	}
}

// TestRigWorkerPoolSlowRigDoesNotBlockOthers verifies that one slow rig does not
// prevent the remaining rigs from completing: the slow rig waits for the fast
// ones, which a pool that ran it first and alone would never let happen
// before its own timeout.
func TestRigWorkerPoolSlowRigDoesNotBlockOthers(t *testing.T) {
	t.Parallel()
	const slowRig = "slow-rig"

	pool := newRigWorkerPool(10, time.Minute, nil)

	var fastDone atomic.Int64
	allFast := make(chan struct{})
	var slowSawFast atomic.Bool

	rigs := []string{slowRig, "fast-1", "fast-2", "fast-3", "fast-4"}
	pool.runPerRig(context.Background(), rigs, func(ctx context.Context, rigName string) error {
		if rigName == slowRig {
			select {
			case <-allFast:
				slowSawFast.Store(true)
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if fastDone.Add(1) == 4 {
			close(allFast)
		}
		return nil
	})

	if got := fastDone.Load(); got != 4 {
		t.Errorf("expected 4 fast rigs to complete, got %d", got)
	}
	if !slowSawFast.Load() {
		t.Error("the fast rigs did not finish while the slow rig was still running")
	}
}
