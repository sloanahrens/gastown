package daemon

import (
	"testing"
	"time"
)

// Regression tests for gt-ayx: a crash-loop flag must self-clear once the
// agent's heartbeat has been continuously fresh with an advancing cycle
// count for the recovery window, instead of requiring a manual
// 'gt daemon clear-backoff'.

func TestObserveHeartbeat_NotInCrashLoop_NoOp(t *testing.T) {
	rt := NewRestartTracker(t.TempDir(), RestartTrackerConfig{})

	if rt.ObserveHeartbeat("deacon", 5, true) {
		t.Fatal("cleared crash loop for an agent that was never flagged")
	}
}

func TestObserveHeartbeat_ClearsAfterSustainedRecovery(t *testing.T) {
	rt := NewRestartTracker(t.TempDir(), RestartTrackerConfig{
		CrashLoopRecoveryWindow: 10 * time.Minute,
	})
	rt.state.Agents["deacon"] = &AgentRestartInfo{
		CrashLoopSince: time.Now().Add(-30 * time.Minute),
	}

	// First fresh+advancing sample starts the recovery window; not enough
	// on its own to clear.
	if rt.ObserveHeartbeat("deacon", 52, true) {
		t.Fatal("cleared crash loop on first recovery sample")
	}
	if !rt.IsInCrashLoop("deacon") {
		t.Fatal("crash loop cleared too early")
	}

	// Cycle advanced, but the recovery window backdated to look like it
	// started 11 minutes ago — sustained recovery, should now auto-clear.
	rt.state.Agents["deacon"].RecoverySince = time.Now().Add(-11 * time.Minute)
	if !rt.ObserveHeartbeat("deacon", 58, true) {
		t.Fatal("did not clear crash loop after sustained recovery")
	}
	if rt.IsInCrashLoop("deacon") {
		t.Fatal("crash loop still set after auto-clear")
	}
}

func TestObserveHeartbeat_StaleHeartbeatResetsWindow(t *testing.T) {
	rt := NewRestartTracker(t.TempDir(), RestartTrackerConfig{
		CrashLoopRecoveryWindow: 10 * time.Minute,
	})
	rt.state.Agents["deacon"] = &AgentRestartInfo{
		CrashLoopSince: time.Now().Add(-30 * time.Minute),
	}

	rt.state.Agents["deacon"].RecoverySince = time.Now().Add(-11 * time.Minute)
	rt.state.Agents["deacon"].RecoveryLastCycle = 52

	// A stale heartbeat must not clear the crash loop even though the
	// recovery window has technically elapsed — the agent must have been
	// continuously alive, not merely alive once 11 minutes ago.
	if rt.ObserveHeartbeat("deacon", 58, false) {
		t.Fatal("cleared crash loop on a stale heartbeat")
	}
	if !rt.IsInCrashLoop("deacon") {
		t.Fatal("crash loop cleared on a stale heartbeat")
	}
	if !rt.state.Agents["deacon"].RecoverySince.IsZero() {
		t.Fatal("recovery window was not reset by stale heartbeat")
	}
}

func TestObserveHeartbeat_StuckCycleResetsWindow(t *testing.T) {
	rt := NewRestartTracker(t.TempDir(), RestartTrackerConfig{
		CrashLoopRecoveryWindow: 10 * time.Minute,
	})
	rt.state.Agents["deacon"] = &AgentRestartInfo{
		CrashLoopSince: time.Now().Add(-30 * time.Minute),
	}

	rt.state.Agents["deacon"].RecoverySince = time.Now().Add(-11 * time.Minute)
	rt.state.Agents["deacon"].RecoveryLastCycle = 52

	// Heartbeat is fresh but the cycle hasn't moved — the deacon is alive
	// but not making progress, so recovery must not count.
	if rt.ObserveHeartbeat("deacon", 52, true) {
		t.Fatal("cleared crash loop while cycle was stuck")
	}
	if !rt.IsInCrashLoop("deacon") {
		t.Fatal("crash loop cleared while cycle was stuck")
	}
}
