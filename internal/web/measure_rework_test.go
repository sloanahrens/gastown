package web

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMeasureBDChildConcurrentAndRender_TimeBeforeAfter is the before/after
// measurement the gt-d5xr rework asked for: how many bd children run at once
// and how long the render takes, unbounded vs bounded. Not a regression test
// — a measurement, so it only logs.
func TestMeasureBDChildConcurrentAndRender_TimeBeforeAfter(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement, skipped under -short")
	}
	const numCalls = 17
	const sleep = 300 * time.Millisecond

	measure := func(t *testing.T, sem chan struct{}) (peak int64, render time.Duration) {
		binDir := t.TempDir()
		runsDir := t.TempDir()
		bdPath := filepath.Join(binDir, "bd")
		writeConcurrencyProbeScript(t, bdPath, runsDir, sleep)

		f := &LiveConvoyFetcher{
			cmdTimeout: 30 * time.Second,
			bdBin:      bdPath,
			cmdSem:     sem,
		}

		stop := make(chan struct{})
		peakPtr, watcherDone := watchPeakConcurrency(runsDir, stop)
		defer func() {
			close(stop)
			<-watcherDone
		}()

		start := time.Now()
		var wg sync.WaitGroup
		wg.Add(numCalls)
		for i := 0; i < numCalls; i++ {
			go func() {
				defer wg.Done()
				if _, err := f.runBdCmd(t.TempDir(), "show", "gt-d5xr"); err != nil {
					t.Errorf("runBdCmd: %v", err)
				}
			}()
		}
		wg.Wait()
		render = time.Since(start)
		return atomic.LoadInt64(peakPtr), render
	}

	unboundedPeak, unboundedRender := measure(t, nil)
	boundedPeak, boundedRender := measure(t, make(chan struct{}, subprocessConcurrency))
	t.Logf("UNBOUNDED (pre-fix): peak bd children = %d, render = %v", unboundedPeak, unboundedRender)
	t.Logf("BOUNDED   (post-fix): peak bd children = %d, render = %v", boundedPeak, boundedRender)
}