package cmd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
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

func TestApplyConfiguredDoltEnvConfigBeatsStaleEnv(t *testing.T) {
	townRoot := t.TempDir()
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  host: 127.0.0.2\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "stale-host")
	t.Setenv("GT_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_PORT", "9999")

	config.ApplyConfiguredDoltEnv(townRoot)

	if got := os.Getenv("GT_DOLT_HOST"); got != "127.0.0.2" {
		t.Fatalf("GT_DOLT_HOST = %q, want 127.0.0.2", got)
	}
	if got := os.Getenv("GT_DOLT_PORT"); got != "5507" {
		t.Fatalf("GT_DOLT_PORT = %q, want 5507", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want cleared", got)
	}
}

func TestApplyConfiguredDoltEnvClearsStaleHostWhenConfigHasNoHost(t *testing.T) {
	townRoot := t.TempDir()
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "stale-host")
	t.Setenv("GT_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")

	config.ApplyConfiguredDoltEnv(townRoot)

	if got := os.Getenv("GT_DOLT_HOST"); got != "" {
		t.Fatalf("GT_DOLT_HOST = %q, want cleared", got)
	}
	if got := os.Getenv("GT_DOLT_PORT"); got != "5507" {
		t.Fatalf("GT_DOLT_PORT = %q, want 5507", got)
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

func TestWaitForDoltReady_ServerListening(t *testing.T) {
	// When server is already listening, should return quickly.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port

	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","dolt_mode":"server","port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_DOLT_PORT", fmt.Sprintf("%d", port))

	start := time.Now()
	waitForDoltReady(townRoot)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("waitForDoltReady took %v, should complete quickly when server is ready", elapsed)
	}
}

func TestWaitForDoltReady_GracefulDegradation(t *testing.T) {
	// Verify that waitForDoltReady doesn't panic or error when Dolt is unreachable.
	// The wrapper should log a warning and continue (graceful degradation).
	// Uses a town root with no server metadata so it returns immediately.
	townRoot := t.TempDir()
	waitForDoltReady(townRoot) // Should not panic

	// Also verify the underlying doltserver.WaitForReady detects unreachable servers.
	// Use bind-then-close to find a guaranteed-free port. (review finding #4)
	tmpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	freePort := tmpListener.Addr().(*net.TCPAddr).Port
	tmpListener.Close()

	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","dolt_mode":"server","port":%d}`, freePort)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_DOLT_PORT", fmt.Sprintf("%d", freePort))

	// WaitForReady with short timeout should fail when nothing is listening
	err = doltserver.WaitForReady(townRoot, 200*time.Millisecond)
	if err == nil {
		t.Error("doltserver.WaitForReady should fail when nothing is listening")
	}
}

func TestWaitForDoltReady_WrapperTimesOutAndContinues(t *testing.T) {
	// Verify the wrapper's error handling path: WaitForReady returns an error
	// but the wrapper logs a warning and continues (doesn't panic or propagate).
	// We test WaitForReady directly with a short timeout to verify the error,
	// since the wrapper uses the package-level 10s constant. (review finding #5)
	tmpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := tmpListener.Addr().(*net.TCPAddr).Port
	tmpListener.Close()

	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","dolt_mode":"server","port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_DOLT_PORT", fmt.Sprintf("%d", port))

	// Verify the error path fires when nothing listens.
	err = doltserver.WaitForReady(townRoot, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected WaitForReady to fail when nothing is listening")
	}
	// Confirm error message is actionable.
	if err.Error() == "" {
		t.Error("expected non-empty error message from WaitForReady timeout")
	}
}
