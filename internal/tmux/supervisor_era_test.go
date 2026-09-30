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

// A pane whose process exited is a confirmed dead agent. Without the
// auto-respawn hook nothing revives it, so reading it as unknown would leave
// the seat alone forever (gt-4k3fj.3, G1-06).
func TestIsAgentAliveChecked_DeadPaneIsDeadNotUnknown(t *testing.T) {
	t.Parallel()
	alive, err := unitTmux(deadPaneServer("1"), nil).IsAgentAliveChecked("gt-x")
	if err != nil || alive {
		t.Fatalf("dead pane: (%v, %v), want (false, nil)", alive, err)
	}
	// The control: a live pane with no answer stays unknown.
	if _, err := unitTmux(deadPaneServer("0"), nil).IsAgentAliveChecked("gt-x"); err == nil {
		t.Fatal("an unanswerable query on a live pane must stay an error")
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
