package daemon

import (
	"testing"
	"time"
)

func TestIsAgentSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"hq-mayor", true},
		{"rig-witness", true},
		{"rig-refinery", false}, // refinery role removed (gt-v4ssj.6)
		{"rig-polecat-abc", true},
		{"hq-deacon", true},
		{"hq-boot", true},
		{"rig-dog-fido", true},
		{"my-personal-session", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isAgentSession(tt.name); got != tt.want {
			t.Errorf("isAgentSession(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// countAgentSessions reads the daemon's own tmux (the town socket), and counts
// only the sessions that look like Gas Town agents.
func TestCountAgentSessionsCountsAgentsOnTheDaemonTmux(t *testing.T) {
	t.Parallel()
	tm := newFakeTmux(newFixedClock())
	for _, name := range []string{"hq-mayor", "rig-witness", "rig-polecat-abc", "my-personal-session"} {
		tm.addSession(name, "claude", time.Time{})
	}
	d := &Daemon{tmux: tm}
	if got := d.countAgentSessions(); got != 3 {
		t.Errorf("countAgentSessions = %d, want 3 (the personal session is not an agent)", got)
	}
}
