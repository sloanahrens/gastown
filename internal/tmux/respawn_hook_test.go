package tmux

import (
	"strings"
	"testing"
)

// TestAutoRespawnHookCmd_Format is a fast unit test verifying the hook command
// string contains all required safety measures.
func TestAutoRespawnHookCmd_Format(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		tmuxCmd  string
		session  string
		wantFlag string
	}{
		{"background_flag", "tmux -L gt", "hq-deacon", "run-shell -b"},
		{"dead_pane_guard", "tmux -L gt", "hq-deacon", "pane_dead"},
		{"error_suppression", "tmux -L gt", "hq-deacon", "|| true"},
		{"socket_in_respawn", "tmux -L gt", "hq-deacon", "-L gt"},
		{"bare_tmux_no_socket", "tmux", "hq-deacon", "tmux respawn-pane"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := buildAutoRespawnHookCmd(tt.tmuxCmd, tt.session)
			if !strings.Contains(cmd, tt.wantFlag) {
				t.Errorf("hook command missing %q:\n  %s", tt.wantFlag, cmd)
			}
		})
	}
}

// TestSetAutoRespawnHookInstallsSocketAwareHook checks what SetAutoRespawnHook
// sends: remain-on-exit first, then a pane-died hook whose embedded tmux calls
// carry this server's -L socket (a bare tmux in run-shell reaches the default
// server instead).
func TestSetAutoRespawnHookInstallsSocketAwareHook(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	if err := unitTmux(s, nil).SetAutoRespawnHook("hq-deacon"); err != nil {
		t.Fatalf("SetAutoRespawnHook: %v", err)
	}
	var remain, hook int = -1, -1
	for i, c := range s.all() {
		switch {
		case c.has("remain-on-exit", "on"):
			remain = i
		case c.sub() == "set-hook" && c.has("-t", "hq-deacon", "pane-died"):
			hook = i
			cmd := c.args[len(c.args)-1]
			if cmd != buildAutoRespawnHookCmd("tmux -L gt-test-unit", "hq-deacon") {
				t.Errorf("hook command = %q, want the socket-aware form", cmd)
			}
		}
	}
	if remain < 0 || hook < 0 || remain > hook {
		t.Fatalf("calls = %v, want remain-on-exit on before the pane-died hook", s.all())
	}
}

func TestSetAutoRespawnHookRejectsBadSessionName(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	if err := unitTmux(s, nil).SetAutoRespawnHook("bad name;rm"); err == nil {
		t.Fatal("SetAutoRespawnHook accepted an invalid session name")
	}
	if len(s.all()) != 0 {
		t.Fatalf("calls = %v, want none for an invalid name", s.all())
	}
}
