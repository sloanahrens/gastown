package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/deacon"
)

func TestDeaconHeartbeatPollOnce_ExitsWhenSessionGone(t *testing.T) {
	townRoot := t.TempDir()

	shouldExit := deaconHeartbeatPollOnce(townRoot, func() bool { return false })
	if !shouldExit {
		t.Fatal("expected shouldExit=true when session is gone")
	}
	if hb := deacon.ReadHeartbeat(townRoot); hb != nil {
		t.Fatalf("expected no heartbeat touch when session is gone, got %+v", hb)
	}
}

// TestDeaconHeartbeatPollOnce_TouchesRepeatedlyWithZeroGtInvocations is the
// direct regression test for gt-x8y: touchDeaconHeartbeat (root.go
// persistentPreRun) only fires when a `gt` command runs, so a long
// bd/git/grep investigative stretch between gt commands previously left the
// heartbeat stale. This test drives the poller's per-tick function directly
// — never calling touchDeaconHeartbeat or any `gt` subcommand — and asserts
// the heartbeat cycle count still advances on every tick, proving the
// background poller is what closes the gap, not incidental gt/bd traffic.
func TestDeaconHeartbeatPollOnce_TouchesRepeatedlyWithZeroGtInvocations(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "deacon"), 0755); err != nil {
		t.Fatal(err)
	}

	const ticks = 4
	var lastCycle int64
	for i := 0; i < ticks; i++ {
		shouldExit := deaconHeartbeatPollOnce(townRoot, func() bool { return true })
		if shouldExit {
			t.Fatalf("tick %d: expected shouldExit=false while session is alive", i)
		}
		hb := deacon.ReadHeartbeat(townRoot)
		if hb == nil {
			t.Fatalf("tick %d: expected heartbeat to be written", i)
		}
		if hb.Cycle <= lastCycle {
			t.Fatalf("tick %d: Cycle = %d, did not advance past %d", i, hb.Cycle, lastCycle)
		}
		lastCycle = hb.Cycle
	}
	if lastCycle != ticks {
		t.Fatalf("final Cycle = %d, want %d after %d ticks", lastCycle, ticks, ticks)
	}
}

func TestDeaconHeartbeatPollOnce_SkipsTouchWhilePausedButKeepsPolling(t *testing.T) {
	townRoot := t.TempDir()
	if err := deacon.Pause(townRoot, "maintenance", "test"); err != nil {
		t.Fatalf("Pause error: %v", err)
	}

	shouldExit := deaconHeartbeatPollOnce(townRoot, func() bool { return true })
	if shouldExit {
		t.Fatal("expected shouldExit=false while paused — poller should keep running, not give up")
	}
	if hb := deacon.ReadHeartbeat(townRoot); hb != nil {
		t.Fatalf("expected no heartbeat touch while paused, got %+v", hb)
	}
}
