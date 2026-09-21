package daemon

import (
	"sync"
	"sync/atomic"
	"testing"
)

// storeTestSlots bounds how many store-backed tests this package runs at once.
//
// Each of the 32 setupTestStore callers opens its own database, runs the full
// beads schema migration (beads@v1.0.5 initSchemaOnDB -> schema.MigrateUp), and
// then polls, creates and closes issues — all against the single Dolt container
// TestMain starts. Run at the Makefile's parallelism (no -parallel, so
// GOMAXPROCS), that load outruns the container: it stops answering for longer
// than the beads pool's 10s read timeout, drops the connections, and tests fail
// on "invalid connection" from CreateIssue and CloseIssue. Measured on the 17
// convoy_manager store tests at -parallel 24: unbounded -> 2 failures in 55s,
// bounded to 4 -> clean in 31s (gt-ihei).
var storeTestSlots = make(chan struct{}, 4)

// takeStoreSlot claims one of storeTestSlots for the calling test and returns
// it when the test ends. Call it once per test, directly after t.Parallel() and
// before any store work: the bound is per test, not per store, because five
// tests here open two or three stores and a token each time would let them wait
// on themselves.
func takeStoreSlot(t *testing.T) {
	t.Helper()
	storeTestSlots <- struct{}{}
	t.Cleanup(func() { <-storeTestSlots })
}

// TestStoreTestSlotsNeverExceedTheBound pins the invariant takeStoreSlot relies
// on: the channel never admits more than its cap. It drives the channel itself
// because the helper's contract is per *testing.T, which a goroutine cannot
// supply. Only the upper bound is asserted — asserting that holders reach the
// cap needs a barrier, since a loaded host starves goroutines before they
// converge (gt-hvzy.2).
func TestStoreTestSlotsNeverExceedTheBound(t *testing.T) {
	const callers = 12
	limit := int64(cap(storeTestSlots))

	var holders, peak, completed atomic.Int64
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			storeTestSlots <- struct{}{}
			defer func() { <-storeTestSlots }()

			n := holders.Add(1)
			for {
				seen := peak.Load()
				if n <= seen || peak.CompareAndSwap(seen, n) {
					break
				}
			}
			holders.Add(-1)
			completed.Add(1)
		}()
	}
	wg.Wait()

	if got := completed.Load(); got != callers {
		t.Errorf("completed %d of %d callers, want all of them", got, callers)
	}
	if got := peak.Load(); got > limit {
		t.Errorf("peak concurrent slot holders %d, want <= %d", got, limit)
	}
}
