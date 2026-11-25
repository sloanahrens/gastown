package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunPatrolScanCheck pins the boot scan (gt-3z6wd5): the first check after a
// boot scans now and never takes the due gate, and every later check takes the
// due gate only.
func TestRunPatrolScanCheck(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		checks    int
		wantNow   int32
		wantDueOn int32
	}{
		{name: "first check scans now and not if due", checks: 1, wantNow: 1, wantDueOn: 0},
		{name: "later checks take the due gate only", checks: 3, wantNow: 1, wantDueOn: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var booted atomic.Bool
			var scannedNow, scannedIfDue atomic.Int32
			for i := 0; i < tc.checks; i++ {
				runPatrolScanCheck(&booted,
					func() { scannedNow.Add(1) },
					func() { scannedIfDue.Add(1) })
			}

			if got := scannedNow.Load(); got != tc.wantNow {
				t.Errorf("scanNow calls = %d, want %d", got, tc.wantNow)
			}
			if got := scannedIfDue.Load(); got != tc.wantDueOn {
				t.Errorf("scanIfDue calls = %d, want %d", got, tc.wantDueOn)
			}
		})
	}
}

// bootRaceBound bounds every wait of the racing test: a racer that panics or is
// never scheduled fails the test with a message at this bound instead of
// hanging it to the package timeout.
const bootRaceBound = 30 * time.Second

// TestRunPatrolScanCheckRacingFirstCall pins the race on the first check: when
// racing callers release together, exactly one scans now and the rest take the
// due gate. The racers start together through a channel closed once all of them
// have arrived, and the test waits for them through a WaitGroup, both under one
// context deadline.
func TestRunPatrolScanCheckRacingFirstCall(t *testing.T) {
	t.Parallel()

	const racers = 16
	ctx, cancel := context.WithTimeout(context.Background(), bootRaceBound)
	defer cancel()

	var booted atomic.Bool
	var scannedNow, scannedIfDue atomic.Int32
	var arrived, finished sync.WaitGroup
	arrived.Add(racers)
	finished.Add(racers)
	release := make(chan struct{})

	for i := 0; i < racers; i++ {
		go func() {
			defer finished.Done()
			arrived.Done()
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			runPatrolScanCheck(&booted,
				func() { scannedNow.Add(1) },
				func() { scannedIfDue.Add(1) })
		}()
	}
	go func() {
		arrived.Wait()
		close(release)
	}()

	done := make(chan struct{})
	go func() {
		finished.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("the %d racers did not all finish within %v (scanNow %d, scanIfDue %d)",
			racers, bootRaceBound, scannedNow.Load(), scannedIfDue.Load())
	}

	if got := scannedNow.Load(); got != 1 {
		t.Errorf("scanNow calls = %d, want exactly 1", got)
	}
	if got := scannedIfDue.Load(); got != racers-1 {
		t.Errorf("scanIfDue calls = %d, want %d", got, racers-1)
	}
}
