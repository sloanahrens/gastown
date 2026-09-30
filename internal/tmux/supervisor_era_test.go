package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deadPaneServer answers like a session whose pane process has exited but
// whose pane was kept (remain-on-exit): no current command, pane_dead=dead.
func deadPaneServer(dead string) *scripted {
	return newScripted(func(c tmuxCall) reply {
		if c.name != "tmux" {
			return ok("")
		}
		switch c.sub() {
		case "show-environment":
			if c.has("GT_PROCESS_NAMES") {
				return ok("GT_PROCESS_NAMES=claude")
			}
			return fail("unknown variable")
		case "display-message":
			if strings.Contains(strings.Join(c.args, " "), "pane_dead") {
				return ok(dead)
			}
			return ok("")
		}
		return ok("")
	})
}

// PaneDead answers for a pane kept after its process exited, which
// IsAgentAliveChecked cannot (no current command): the liveness function
// uses it to call such a seat Dead instead of Unknown forever.
func TestPaneDead(t *testing.T) {
	t.Parallel()
	for answer, want := range map[string]bool{"1": true, "0": false} {
		dead, err := unitTmux(deadPaneServer(answer), nil).PaneDead("gt-x")
		if err != nil || dead != want {
			t.Errorf("pane_dead=%s: (%v, %v), want (%v, nil)", answer, dead, err, want)
		}
	}
	// The agent query itself still reports the failure rather than guessing.
	if _, err := unitTmux(deadPaneServer("1"), nil).IsAgentAliveChecked("gt-x"); err == nil {
		t.Error("IsAgentAliveChecked on a dead pane must keep its error; PaneDead is the answer")
	}
	s := newScripted(bySub(map[string]reply{"display-message": fail("server exited")}))
	if _, err := unitTmux(s, nil).PaneDead("gt-x"); err == nil {
		t.Error("a failed pane_dead query must be an error")
	}
}

// G1-20: the split-brain sweep kills a same-named default-socket session
// only when it is a town session (it carries GT_ROLE). An operator's own
// session that happens to share the name is left alone.
func TestKillSplitBrainSessionSparesANonTownSession(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"show-environment": fail("unknown variable: GT_ROLE")}))
	unitTmux(s, nil).killSplitBrainSession("gt-x")
	if kills := s.find("kill-session"); len(kills) != 0 {
		t.Fatalf("kill-session calls = %v, want none for a session without GT_ROLE", kills)
	}
}

// G1-06: the tmux pane-died auto-respawn hook is gone. Nothing restarts a
// session outside the supervisor.
func TestNoAutoRespawnHookInTheTree(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, banned := range []string{"SetAutoRespawnHook", "buildAutoRespawnHookCmd", "AutoRespawn:"} {
			if strings.Contains(string(data), banned) {
				t.Errorf("%s contains %q", path, banned)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
