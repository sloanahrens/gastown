package tmux

import (
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
