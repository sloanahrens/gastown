package refinery

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// These tests pin the keep-or-kill policy that fixed the refinery respawn
// burst (gt-uj9k): 4-8 session_starts in ~90s, because a single non-error-aware
// liveness probe reported booting or unqueryable sessions as dead and the start
// path killed them.

// TestDecideSessionReconcile_LivenessErrorIsNotDeath is the central regression
// test. IsAgentAlive swallows its own query errors, so a transient tmux failure
// used to arrive here as "agent dead" and the caller killed a healthy session.
// Unknown state must never authorize a kill.
func TestDecideSessionReconcile_LivenessErrorIsNotDeath(t *testing.T) {
	t.Parallel()
	outcome, reason, detail := DecideSessionReconcile(SessionReconcileFacts{
		LivenessKnown:  false,
		LivenessDetail: "tmux show-environment: server exited unexpectedly",
	})

	if outcome != reconcileKeep {
		t.Fatalf("outcome = %v, want reconcileKeep — an unqueryable session is not a dead one", outcome)
	}
	if reason != "keep-liveness-unknown" {
		t.Errorf("reason = %q, want keep-liveness-unknown", reason)
	}
	if detail == "" {
		t.Error("detail is empty; the probe error must reach the decision log")
	}
}

// TestDecideSessionReconcile_BootingSessionIsNotAZombie covers the case that
// made the burst the common path rather than a race: Claude's process tree is
// undetectable for the whole bootstrap window, so every concurrent start path
// (daemon heartbeat, gt up, gt start --all, patrol formulas) saw the session
// its predecessor had just created as a zombie and killed it. One age is
// absolute (ClaudeStartTimeout-1s), so a grace shorter than the window fails
// here instead of only at its own moving boundary.
func TestDecideSessionReconcile_BootingSessionIsNotAZombie(t *testing.T) {
	t.Parallel()
	for _, age := range []time.Duration{0, time.Second, 10 * time.Second, time.Minute, constants.ClaudeStartTimeout - time.Second, constants.SessionBootGracePeriod - time.Millisecond} {
		outcome, reason, _ := DecideSessionReconcile(SessionReconcileFacts{
			LivenessKnown:   true,
			Alive:           false,
			SessionAgeKnown: true,
			SessionAge:      age,
		})
		if outcome != reconcileKeep {
			t.Errorf("age %v: outcome = %v, want reconcileKeep", age, outcome)
		}
		if reason != "keep-booting" {
			t.Errorf("age %v: reason = %q, want keep-booting", age, reason)
		}
	}
}

// TestDecideSessionReconcile_AbstainsWhileSuiteRuns pins the gate-awareness
// requirement: a restart must never land mid-suite and lose the run.
func TestDecideSessionReconcile_AbstainsWhileSuiteRuns(t *testing.T) {
	t.Parallel()
	outcome, reason, detail := DecideSessionReconcile(SessionReconcileFacts{
		LivenessKnown:   true,
		Alive:           false,
		SessionAgeKnown: true,
		SessionAge:      time.Hour,
		SuiteBusy:       true,
		SuiteDetail:     "container-gate slot held by gastown/refinery-batch",
	})

	if outcome != reconcileKeep {
		t.Fatalf("outcome = %v, want reconcileKeep while a gate suite holds the slot", outcome)
	}
	if reason != "keep-suite-running" {
		t.Errorf("reason = %q, want keep-suite-running", reason)
	}
	if detail == "" {
		t.Error("detail is empty; the slot-busy cause must reach the decision log")
	}
}

// TestDecideSessionReconcile_SuiteGateBeatsBootGrace documents the precedence:
// a long-lived session with a suite running is kept by the suite gate, not by
// the boot grace, so the log names the real reason.
func TestDecideSessionReconcile_SuiteGateBeatsBootGrace(t *testing.T) {
	t.Parallel()
	outcome, reason, _ := DecideSessionReconcile(SessionReconcileFacts{
		LivenessKnown:   true,
		Alive:           false,
		SessionAgeKnown: true,
		SessionAge:      time.Second,
		SuiteBusy:       true,
	})

	if outcome != reconcileKeep {
		t.Fatalf("outcome = %v, want reconcileKeep", outcome)
	}
	if reason != "keep-booting" {
		t.Errorf("reason = %q, want keep-booting (boot grace is checked first)", reason)
	}
}

// TestDecideSessionReconcile_RecheckKeepsRecoveredSession covers the TOCTOU
// re-verification: an agent that finishes booting during the grace period is
// kept rather than killed.
func TestDecideSessionReconcile_RecheckKeepsRecoveredSession(t *testing.T) {
	t.Parallel()
	outcome, reason, _ := DecideSessionReconcile(SessionReconcileFacts{
		LivenessKnown:   true,
		Alive:           false,
		SessionAgeKnown: true,
		SessionAge:      time.Hour,
		AliveAfterGrace: true,
	})

	if outcome != reconcileKeep {
		t.Fatalf("outcome = %v, want reconcileKeep — the agent recovered during the grace period", outcome)
	}
	if reason != "keep-recovered" {
		t.Errorf("reason = %q, want keep-recovered", reason)
	}
}

// TestDecideSessionReconcile_RecheckKeepsReplacedSession covers the second
// TOCTOU branch: another caller already replaced the session, so killing now
// would destroy that caller's work.
func TestDecideSessionReconcile_RecheckKeepsReplacedSession(t *testing.T) {
	t.Parallel()
	outcome, reason, _ := DecideSessionReconcile(SessionReconcileFacts{
		LivenessKnown:   true,
		Alive:           false,
		SessionAgeKnown: true,
		SessionAge:      time.Hour,
		SessionReplaced: true,
	})

	if outcome != reconcileKeep {
		t.Fatalf("outcome = %v, want reconcileKeep — another caller owns the new session", outcome)
	}
	if reason != "keep-session-replaced" {
		t.Errorf("reason = %q, want keep-session-replaced", reason)
	}
}

// TestDecideSessionReconcile_HealthySessionIsKept is the ordinary case: the
// policy must not have become so conservative that it churns live refineries.
func TestDecideSessionReconcile_HealthySessionIsKept(t *testing.T) {
	t.Parallel()
	outcome, reason, _ := DecideSessionReconcile(SessionReconcileFacts{
		LivenessKnown: true,
		Alive:         true,
	})

	if outcome != reconcileKeep {
		t.Fatalf("outcome = %v, want reconcileKeep", outcome)
	}
	if reason != "keep-alive" {
		t.Errorf("reason = %q, want keep-alive", reason)
	}
}

// TestDecideSessionReconcile_GenuineZombieIsKilled guards the other direction:
// a session well past the boot grace, whose agent is confirmed dead, with no
// suite running, must still be replaced — otherwise recovery would be lost.
func TestDecideSessionReconcile_GenuineZombieIsKilled(t *testing.T) {
	t.Parallel()
	outcome, reason, detail := DecideSessionReconcile(SessionReconcileFacts{
		LivenessKnown:   true,
		Alive:           false,
		SessionAgeKnown: true,
		SessionAge:      time.Hour,
	})

	if outcome != reconcileKill {
		t.Fatalf("outcome = %v, want reconcileKill for a confirmed zombie", outcome)
	}
	if reason != "kill-zombie" {
		t.Errorf("reason = %q, want kill-zombie", reason)
	}
	if detail == "" {
		t.Error("detail is empty; the kill rationale must reach the decision log")
	}
}

// TestDecideSessionReconcile_UnknownAgeStillKills documents that an unreadable
// creation time does not block recovery: only an unreadable *liveness* probe
// is treated as unknown, because only that one is ambiguous between "booting"
// and "dead".
func TestDecideSessionReconcile_UnknownAgeStillKills(t *testing.T) {
	t.Parallel()
	outcome, _, _ := DecideSessionReconcile(SessionReconcileFacts{
		LivenessKnown:   true,
		Alive:           false,
		SessionAgeKnown: false,
	})

	if outcome != reconcileKill {
		t.Fatalf("outcome = %v, want reconcileKill when liveness is known-dead", outcome)
	}
}

// TestTwoConcurrentStartersDoNotPingPong is the end-to-end shape of the bug:
// two callers arriving within the boot grace must both abstain, so neither
// kills the session the other created. Before the fix the second caller killed
// and recreated, which emitted a second session_start — repeated across callers
// and daemon restarts, that is the 4-8-in-90s burst.
func TestTwoConcurrentStartersDoNotPingPong(t *testing.T) {
	t.Parallel()
	// Caller A creates the session; caller B arrives 3s later while Claude is
	// still booting and cannot be detected yet.
	bFacts := SessionReconcileFacts{
		LivenessKnown:   true,
		Alive:           false,
		SessionAgeKnown: true,
		SessionAge:      3 * time.Second,
	}

	if outcome, _, _ := DecideSessionReconcile(bFacts); outcome != reconcileKeep {
		t.Fatalf("caller B outcome = %v, want reconcileKeep — B must not kill A's booting session", outcome)
	}

	// And a third caller arriving after the grace period, still unable to see
	// the agent, re-checks and finds it alive.
	thirdFacts := bFacts
	thirdFacts.SessionAge = 2 * constants.SessionBootGracePeriod
	thirdFacts.AliveAfterGrace = true

	if outcome, _, _ := DecideSessionReconcile(thirdFacts); outcome != reconcileKeep {
		t.Fatalf("third caller outcome = %v, want reconcileKeep after post-grace re-check", outcome)
	}
}
