//go:build !windows

package refinery

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/util"
)

// TestRunGate_TimeoutKillsTheWholeGroup guards gt-6t43 for the refinery's gate
// runner: a gate killed at its per-gate timeout must take the processes it
// started with it.
//
// The gate command here has the shape of a real rig gate — sh → make → test
// binary — reduced to what a test can assert on: a backgrounded grandchild
// that heartbeats and then drops a marker file. Killing only the shell, which
// is everything a process group without a Cancel hook reaches, leaves the
// grandchild running in the group the gate has already released (gt-ypkc
// fixed the two `gt done` call sites; the refinery's three were still on the
// old helper).
func TestRunGate_TimeoutKillsTheWholeGroup(t *testing.T) {
	// Not parallel: this test writes util.ProcessGroupKillGrace, and parallel
	// siblings would race it.
	stubProcessGroupKillGrace(t, 50*time.Millisecond)

	dir := t.TempDir()
	e := NewEngineer(&rig.Rig{Name: "test-rig", Path: dir})
	e.workDir = dir
	e.output = io.Discard

	beat := filepath.Join(dir, "beat")
	orphan := filepath.Join(dir, "orphan-ran")
	// 30 heartbeats at 100ms outlive every assertion below by a wide margin,
	// so a surviving grandchild is unambiguous rather than merely slow.
	cmd := fmt.Sprintf("(i=0; while [ $i -lt 30 ]; do echo x >> %q; i=$((i+1)); sleep 0.1; done; touch %q) & wait",
		beat, orphan)

	start := time.Now()
	result := e.runGate(context.Background(), "slow", &GateConfig{
		Cmd:     cmd,
		Timeout: 250 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if result.Success {
		t.Errorf("success = true, want false for a gate killed at its timeout")
	}
	if result.Error != "timed out after 250ms" {
		t.Errorf("error = %q, want the timeout attributed to the gate's own budget", result.Error)
	}
	// The grandchild holds the gate's stdout/stderr pipes open, so a kill that
	// reaches only the shell makes Wait block on those pipes until the
	// grandchild finishes: the gate then returns when the orphan decides to,
	// not when its budget says so.
	if elapsed > 2*time.Second {
		t.Errorf("runGate returned after %s, not within its 250ms budget: the timeout killed the shell while its children kept the gate's pipes open", elapsed.Round(time.Millisecond))
	}

	before := beatSize(t, beat)
	time.Sleep(500 * time.Millisecond)
	if after := beatSize(t, beat); after != before {
		t.Errorf("the gate's grandchild kept running after the gate timed out (heartbeat grew %d → %d bytes)", before, after)
	}
	if _, err := os.Stat(orphan); err == nil {
		t.Error("the gate's grandchild ran to completion after the gate timed out — the timeout orphaned it")
	}
}

// TestRunTests_TimeoutKillsTheWholeGroup is the same guard for the other
// refinery gate call site, where the deadline comes from the caller's context
// rather than per-gate config, and where the caller retries on failure: an
// orphaned suite from attempt 1 competes with attempt 2 for the same
// containers.
func TestRunTests_TimeoutKillsTheWholeGroup(t *testing.T) {
	// Not parallel: this test writes util.ProcessGroupKillGrace, and parallel
	// siblings would race it.
	stubProcessGroupKillGrace(t, 50*time.Millisecond)

	dir := t.TempDir()
	e := NewEngineer(&rig.Rig{Name: "test-rig", Path: dir})
	e.workDir = dir
	e.output = io.Discard

	beat := filepath.Join(dir, "beat")
	orphan := filepath.Join(dir, "orphan-ran")
	e.config.TestCommand = fmt.Sprintf("(i=0; while [ $i -lt 30 ]; do echo x >> %q; i=$((i+1)); sleep 0.1; done; touch %q) & wait",
		beat, orphan)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := e.runTests(ctx)
	elapsed := time.Since(start)

	if result.Success {
		t.Errorf("success = true, want false for a suite killed at its deadline")
	}
	if elapsed > 2*time.Second {
		t.Errorf("runTests returned after %s, not within its 250ms deadline: the timeout killed the shell while its children kept the gate's pipes open", elapsed.Round(time.Millisecond))
	}

	before := beatSize(t, beat)
	time.Sleep(500 * time.Millisecond)
	if after := beatSize(t, beat); after != before {
		t.Errorf("the suite's grandchild kept running after the deadline (heartbeat grew %d → %d bytes)", before, after)
	}
	if _, err := os.Stat(orphan); err == nil {
		t.Error("the suite's grandchild ran to completion after the deadline — the timeout orphaned it")
	}
}

func beatSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// stubProcessGroupKillGrace shrinks the SIGTERM grace the gate kills wait out,
// so these tests measure the timeout path rather than the grace period. The
// assertion they carry is that the kill takes the group, not how long the
// kernel takes to reap it.
func stubProcessGroupKillGrace(t *testing.T, grace time.Duration) {
	t.Helper()
	prev := util.ProcessGroupKillGrace
	util.ProcessGroupKillGrace = grace
	t.Cleanup(func() { util.ProcessGroupKillGrace = prev })
}
