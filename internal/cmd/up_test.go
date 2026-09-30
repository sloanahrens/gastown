package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/rig"
)

func TestAgentStartResult_Fields(t *testing.T) {
	t.Parallel()
	result := agentStartResult{
		name:   "Witness (gastown)",
		ok:     true,
		detail: "gt-gastown-witness",
	}

	if result.name != "Witness (gastown)" {
		t.Errorf("name = %q, want %q", result.name, "Witness (gastown)")
	}
	if !result.ok {
		t.Error("ok should be true")
	}
	if result.detail != "gt-gastown-witness" {
		t.Errorf("detail = %q, want %q", result.detail, "gt-gastown-witness")
	}
}

func TestMaxConcurrentAgentStarts_Constant(t *testing.T) {
	t.Parallel()
	// Verify the constant is set to a reasonable value
	if maxConcurrentAgentStarts < 1 {
		t.Errorf("maxConcurrentAgentStarts = %d, should be >= 1", maxConcurrentAgentStarts)
	}
	if maxConcurrentAgentStarts > 100 {
		t.Errorf("maxConcurrentAgentStarts = %d, should be <= 100 to prevent resource exhaustion", maxConcurrentAgentStarts)
	}
}

func TestSemaphoreLimitsConcurrency(t *testing.T) {
	t.Parallel()
	// Test that a semaphore pattern properly limits concurrency
	const maxConcurrent = 3
	const totalTasks = 10

	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	var maxObserved int32
	var current int32

	for i := 0; i < totalTasks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Acquire semaphore
			sem <- struct{}{}
			defer func() { <-sem }()

			// Track concurrent count
			cur := atomic.AddInt32(&current, 1)
			defer atomic.AddInt32(&current, -1)

			// Update max observed
			for {
				max := atomic.LoadInt32(&maxObserved)
				if cur <= max || atomic.CompareAndSwapInt32(&maxObserved, max, cur) {
					break
				}
			}

			// Simulate work
			time.Sleep(10 * time.Millisecond)
		}()
	}

	wg.Wait()

	if maxObserved > maxConcurrent {
		t.Errorf("max concurrent = %d, should not exceed %d", maxObserved, maxConcurrent)
	}
}

func TestStartRigAgentsWithPrefetch_EmptyRigs(t *testing.T) {
	t.Parallel()
	// Test with empty inputs
	witnessResults := startRigAgentsWithPrefetch(
		[]string{},
		make(map[string]*rig.Rig),
		make(map[string]error),
	)

	if len(witnessResults) != 0 {
		t.Errorf("witnessResults should be empty, got %d entries", len(witnessResults))
	}
}

func TestStartRigAgentsWithPrefetch_RecordsErrors(t *testing.T) {
	t.Parallel()
	// Test that rig errors are properly recorded
	rigErrors := map[string]error{
		"badrig": fmt.Errorf("rig not found"),
	}

	witnessResults := startRigAgentsWithPrefetch(
		[]string{"badrig"},
		make(map[string]*rig.Rig),
		rigErrors,
	)

	if len(witnessResults) != 1 {
		t.Errorf("witnessResults should have 1 entry, got %d", len(witnessResults))
	}
	if result, ok := witnessResults["badrig"]; !ok {
		t.Error("witnessResults should have badrig entry")
	} else if result.ok {
		t.Error("badrig witness result should not be ok")
	}
}

func TestPrefetchRigs_Empty(t *testing.T) {
	t.Parallel()
	// Test with empty rig list
	rigs, errors := prefetchRigs([]string{})

	if len(rigs) != 0 {
		t.Errorf("rigs should be empty, got %d entries", len(rigs))
	}
	if len(errors) != 0 {
		t.Errorf("errors should be empty, got %d entries", len(errors))
	}
}

func TestWorkerPoolLimitsConcurrency(t *testing.T) {
	t.Parallel()
	// Test that a worker pool pattern properly limits concurrency
	const numWorkers = 3
	const numTasks = 15

	tasks := make(chan int, numTasks)
	results := make(chan int, numTasks)

	var maxObserved int32
	var current int32

	// Start worker pool
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range tasks {
				// Track concurrent count
				cur := atomic.AddInt32(&current, 1)

				// Update max observed
				for {
					max := atomic.LoadInt32(&maxObserved)
					if cur <= max || atomic.CompareAndSwapInt32(&maxObserved, max, cur) {
						break
					}
				}

				// Simulate work
				time.Sleep(5 * time.Millisecond)

				atomic.AddInt32(&current, -1)
				results <- 1
			}
		}()
	}

	// Enqueue tasks
	for i := 0; i < numTasks; i++ {
		tasks <- i
	}
	close(tasks)

	// Wait for workers and collect results
	go func() {
		wg.Wait()
		close(results)
	}()

	count := 0
	for range results {
		count++
	}

	if count != numTasks {
		t.Errorf("expected %d results, got %d", numTasks, count)
	}
	if maxObserved > numWorkers {
		t.Errorf("max concurrent = %d, should not exceed %d workers", maxObserved, numWorkers)
	}
}

// =============================================================================
// waitForDoltReady tests (gt-zou1n)
// Verifies that gt up waits for Dolt server readiness before starting witnesses.
// =============================================================================

// =============================================================================
// recoverOrphanedBeads tests (gas-udp)
// Verifies that gt up detects and recovers orphaned hooked beads after crash.
// =============================================================================

func TestRecoverOrphanedBeads_NoRigs(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	services := recoverOrphanedBeads(townRoot, []string{}, make(map[string]*rig.Rig))
	if len(services) != 0 {
		t.Errorf("expected no services, got %d", len(services))
	}
}

func TestRecoverOrphanedBeads_SkipsUnloadedRigs(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// Rig "badrig" is in the list but not in prefetchedRigs — should be skipped.
	services := recoverOrphanedBeads(townRoot, []string{"badrig"}, make(map[string]*rig.Rig))
	if len(services) != 0 {
		t.Errorf("expected no services for unloaded rig, got %d", len(services))
	}
}

func TestRecoverOrphanedBeads_NoOrphansCleanRig(t *testing.T) {
	t.Parallel()
	// Set up a rig directory with no beads — should produce no services.
	// Note: Full recovery-path tests (hooked bead + dead polecat → reset to open)
	// require a live Dolt server and are covered by DetectOrphanedBeads tests
	// in internal/witness/handlers_test.go. These up_test.go tests verify the
	// integration wiring: correct rig iteration, skip logic, and service reporting.
	townRoot := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, "polecats"), 0755); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Path: rigPath}
	prefetched := map[string]*rig.Rig{rigName: r}
	services := recoverOrphanedBeads(townRoot, []string{rigName}, prefetched)
	if len(services) != 0 {
		t.Errorf("expected no services for clean rig, got %d", len(services))
	}
}

func TestRecoverOrphanedBeads_MultipleRigsOnlyProcessesLoaded(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Set up two rigs, but only prefetch one
	for _, name := range []string{"rig-a", "rig-b"} {
		rigPath := filepath.Join(townRoot, name)
		if err := os.MkdirAll(filepath.Join(rigPath, "polecats"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	prefetched := map[string]*rig.Rig{
		"rig-a": {Path: filepath.Join(townRoot, "rig-a")},
		// rig-b intentionally not prefetched
	}
	services := recoverOrphanedBeads(townRoot, []string{"rig-a", "rig-b"}, prefetched)
	// Neither rig should have orphans (no Dolt server = no beads found),
	// but the function should complete without error and not panic on rig-b.
	for _, svc := range services {
		if svc.Rig == "rig-b" {
			t.Errorf("rig-b should have been skipped (not prefetched), but got service: %s", svc.Detail)
		}
	}
}

func TestWaitForDoltReady_NoServerMode(t *testing.T) {
	t.Parallel()
	// When no server mode metadata exists, waitForDoltReady should not block.
	townRoot := t.TempDir()

	start := time.Now()
	waitForDoltReady(townRoot)
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Errorf("waitForDoltReady took %v with no server mode, should return immediately", elapsed)
	}
}
