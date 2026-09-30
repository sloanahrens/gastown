package tmux

import "testing"

// TestKillSplitBrainSessionUsesInjectedRunner pins that the default-socket
// probe goes through the same runner (and so the same clock and process
// seam) as the Tmux that asked for it, rather than a fresh real one.
func TestKillSplitBrainSessionUsesInjectedRunner(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"has-session": fail("can't find session: gt-x")}))
	unitTmux(s, nil).killSplitBrainSession("gt-x")

	calls := s.find("has-session")
	if len(calls) != 1 || calls[0].socket != "default" || !calls[0].has("-t", "=gt-x") {
		t.Fatalf("has-session calls = %v, want one on socket default for =gt-x", calls)
	}
}

// TestKillSplitBrainSessionKillsStaleDefaultSession: a same-named town
// session (it carries GT_ROLE) on the default socket is killed there.
func TestKillSplitBrainSessionKillsStaleDefaultSession(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{
		"display-message":  ok(""),
		"show-environment": ok("GT_ROLE=gastown/witness"),
	}))
	unitTmux(s, nil).killSplitBrainSession("gt-x")

	kills := s.find("kill-session")
	if len(kills) != 1 || kills[0].socket != "default" {
		t.Fatalf("kill-session calls = %v, want one on socket default", kills)
	}
}

func TestKillSplitBrainSessionSkipsDefaultSocket(t *testing.T) {
	t.Parallel()
	for _, sock := range []string{"", "default", noTownSocket} {
		s := newScripted(nil)
		newTmuxForTest(sock, s.exec, nil).killSplitBrainSession("gt-x")
		if got := s.all(); len(got) != 0 {
			t.Errorf("socket %q: calls = %v, want none", sock, got)
		}
	}
}
