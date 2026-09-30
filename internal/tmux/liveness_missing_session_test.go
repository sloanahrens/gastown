package tmux

import "testing"

// TestIsAgentAliveChecked_MissingSessionIsDead is gt-7jblf: a session that no
// longer exists has no agent in it, so the liveness question has a positive
// answer — dead — and must not come back Unknown.
//
// The patrol watchdog reads an unknown answer as alive and judges the patrol by
// its receipts (internal/daemon/patrol_watchdog.go), so an exited session used
// to be reported as a live one that had never completed a patrol cycle
// (gt-jv0k3).
func TestIsAgentAliveChecked_MissingSessionIsDead(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-other", "claude") // a server that is up, without this session
	tm, _ := f.tmux(nil)

	alive, err := tm.IsAgentAliveChecked("gt-missing")
	if err != nil {
		t.Fatalf("IsAgentAliveChecked(missing session) = %v, %v; want false, nil", alive, err)
	}
	if alive {
		t.Fatal("a session that does not exist reported a live agent")
	}
	if got := tm.CheckSessionHealth("gt-missing", 0); got != SessionDead {
		t.Errorf("CheckSessionHealth(missing session) = %v, want %v", got, SessionDead)
	}
}

// TestIsAgentAliveChecked_NoServerIsDead: a tmux server that is down holds no
// session either, so the same positive answer applies. HasSession classifies
// ErrNoServer, which is why the existence probe reads it as dead rather than
// letting it reach the environment queries as an unclassified error.
func TestIsAgentAliveChecked_NoServerIsDead(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-x", "claude")
	tm, _ := f.tmux(nil)
	f.with(func() { f.noServer = true })

	if alive, err := tm.IsAgentAliveChecked("gt-x"); err != nil || alive {
		t.Fatalf("IsAgentAliveChecked with no tmux server = %v, %v; want false, nil", alive, err)
	}
}

// TestIsAgentAliveChecked_UnanswerableExistenceIsUnknown holds the gt-fcxe9.1
// line for the new probe: a has-session that cannot be answered is Unknown, so
// no caller acts on it. Only a session tmux positively denies reads as dead.
func TestIsAgentAliveChecked_UnanswerableExistenceIsUnknown(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"has-session": fail("timed out waiting for server")}))
	alive, err := unitTmux(s, nil).IsAgentAliveChecked("gt-x")
	if err == nil {
		t.Fatalf("IsAgentAliveChecked = %v, nil; want an error when has-session cannot be answered", alive)
	}
	if alive {
		t.Fatal("an unanswerable existence probe reported a live agent")
	}
}
