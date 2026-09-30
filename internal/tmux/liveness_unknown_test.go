package tmux

import (
	"errors"
	"testing"
)

// failingShowEnv answers like f, except that every show-environment call
// fails the way a tmux query does when the host is saturated. The session and
// its agent process are really there; only the question cannot be answered.
func failingShowEnv(f *fakeServer) (*Tmux, *scripted) {
	s := newScripted(func(c tmuxCall) reply {
		if c.name == "tmux" && c.sub() == "show-environment" {
			return fail("timed out waiting for server")
		}
		return f.answer(c)
	})
	return unitTmux(s, nil), s
}

// TestCheckSessionHealth_LivenessErrorIsUnknown is gt-fcxe9.1: a failed liveness
// query must read as AgentUnknown, which is not a zombie, never as AgentDead.
func TestCheckSessionHealth_LivenessErrorIsUnknown(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-live", "claude")
	tm, _ := failingShowEnv(f)

	got := tm.CheckSessionHealth("gt-live", 0)
	if got != AgentUnknown {
		t.Fatalf("CheckSessionHealth = %v, want %v", got, AgentUnknown)
	}
	if got.IsZombie() {
		t.Fatal("AgentUnknown must not count as a zombie")
	}
}

// TestCleanupOrphanedSessions_LivenessErrorKillsNothing: the sweep kills only
// sessions confirmed dead; an unanswerable liveness query leaves them alone.
func TestCleanupOrphanedSessions_LivenessErrorKillsNothing(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-live", "claude")
	tm, _ := failingShowEnv(f)

	cleaned, err := tm.CleanupOrphanedSessions(func(string) bool { return true })
	if err != nil {
		t.Fatalf("CleanupOrphanedSessions: %v", err)
	}
	if cleaned != 0 || !f.has("gt-live") {
		t.Fatalf("cleaned=%d, session present=%v; want 0 and true", cleaned, f.has("gt-live"))
	}
}

// TestZombieStatus_CountsAsRunning pins the rule every IsRunning wrapper
// (polecat, witness, refinery, feed) uses: unknown counts as running, so a
// failed query never leads a caller to start a second agent into the session.
func TestZombieStatus_CountsAsRunning(t *testing.T) {
	t.Parallel()
	for status, want := range map[ZombieStatus]bool{
		SessionHealthy: true,
		AgentUnknown:   true,
		SessionDead:    false,
		AgentDead:      false,
		AgentHung:      false,
	} {
		if got := status.CountsAsRunning(); got != want {
			t.Errorf("%v.CountsAsRunning() = %v, want %v", status, got, want)
		}
	}
}

// TestIsAgentAliveChecked_SessionVanishingMidQueryIsNotFound pins the part of
// gt-jv0k3 that survives gt-7jblf. A session tmux denies up front reads dead,
// (false, nil) — TestIsAgentAliveChecked_MissingSessionIsDead — so the only way
// to reach show-environment for a missing session is to lose it between
// has-session and the environment read. tmux 3.7c words that miss "no such
// session", and it has to reach the caller as ErrSessionNotFound rather than
// an unclassified error, so a reader can tell a gone session from a query that
// failed (the patrol watchdog does). TestWrapError pins the wording and
// TestIntegrationFakeServerMatchesTmux replays it against real tmux.
func TestIsAgentAliveChecked_SessionVanishingMidQueryIsNotFound(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{
		"show-environment": fail("no such session: gt-gone"),
	}))

	alive, err := unitTmux(s, nil).IsAgentAliveChecked("gt-gone")
	if alive {
		t.Fatal("a session that vanished mid-query reported a live agent")
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("IsAgentAliveChecked on a vanished session = %v, want ErrSessionNotFound", err)
	}
}
