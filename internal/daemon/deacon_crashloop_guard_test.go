package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/session"
)

// Regression test for gt-d61:
// even when Deacon is in crash-loop state, stale-heartbeat fallback still kills session.
func TestCheckDeaconHeartbeat_RespectsCrashLoopGuard(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Stale heartbeat triggers restart path.
	if err := deacon.WriteHeartbeat(townRoot, &deacon.Heartbeat{
		Timestamp: time.Now().Add(-20 * time.Minute),
		Cycle:     1,
	}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	rt := NewRestartTracker(townRoot, RestartTrackerConfig{})
	rt.state.Agents["deacon"] = &AgentRestartInfo{
		CrashLoopSince: time.Now().Add(-5 * time.Minute),
	}

	d, tm, clk := newDeaconHeartbeatDaemon(t, townRoot, nil)
	d.restartTracker = rt
	respawned := false
	d.startDeaconFn = func() error { respawned = true; return nil }

	runOnClock(t, clk, time.Second, d.checkDeaconHeartbeat)

	if has, _ := tm.HasSession(session.DeaconSessionName()); !has || respawned {
		t.Fatalf("Deacon session live=%v respawned=%v, want it left alone while the crash-loop guard is active", has, respawned)
	}
}

// Regression test for gt-ayx: a healthy Deacon (fresh heartbeat, advancing
// cycles) must have its crash-loop flag auto-cleared instead of staying
// flagged until a human runs 'gt daemon clear-backoff'.
func TestCheckDeaconHeartbeat_AutoClearsOnSustainedRecovery(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	if err := deacon.WriteHeartbeat(townRoot, &deacon.Heartbeat{
		Timestamp: time.Now(),
		Cycle:     58,
	}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	rt := NewRestartTracker(townRoot, RestartTrackerConfig{
		CrashLoopRecoveryWindow: 10 * time.Minute,
	})
	rt.state.Agents["deacon"] = &AgentRestartInfo{
		CrashLoopSince:    time.Now().Add(-30 * time.Minute),
		RecoverySince:     time.Now().Add(-11 * time.Minute),
		RecoveryLastCycle: 52,
	}

	d, _, clk := newDeaconHeartbeatDaemon(t, townRoot, nil)
	d.restartTracker = rt

	runOnClock(t, clk, time.Second, d.checkDeaconHeartbeat)

	if rt.IsInCrashLoop("deacon") {
		t.Fatal("crash loop was not auto-cleared after sustained recovery")
	}
}
