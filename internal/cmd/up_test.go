package cmd

import (
	"testing"
	"time"
)

func TestAgentStartResult_Fields(t *testing.T) {
	t.Parallel()
	result := agentStartResult{
		name:   "Mayor",
		ok:     true,
		detail: "hq-mayor",
	}

	if result.name != "Mayor" {
		t.Errorf("name = %q, want %q", result.name, "Mayor")
	}
	if !result.ok {
		t.Error("ok should be true")
	}
	if result.detail != "hq-mayor" {
		t.Errorf("detail = %q, want %q", result.detail, "hq-mayor")
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

// =============================================================================
// waitForDoltReady tests (gt-zou1n)
// Verifies that gt up waits for Dolt server readiness before starting agents.
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
