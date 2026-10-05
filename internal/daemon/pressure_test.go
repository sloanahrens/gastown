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
		{"hq-mayor", false}, // mayor role retired (gt-rwp7z.17)
		{"rig-crew-max", true},
		{"rig-refinery", false}, // refinery role removed (gt-v4ssj.6)
		{"rig-witness", false},  // witness, deacon and boot roles retired (gt-4k3fj.6.1)
		{"rig-polecat-abc", true},
		{"hq-deacon", false},
		{"hq-boot", false},
		{"hq-dog-fido", false}, // dog role retired (gt-29q6g)
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
	for _, name := range []string{"rig-crew-max", "rig-polecat-abc", "my-personal-session"} {
		tm.addSession(name, "claude", time.Time{})
	}
	d := &Daemon{tmux: tm}
	if got := d.countAgentSessions(); got != 2 {
		t.Errorf("countAgentSessions = %d, want 2 (the personal session is not an agent)", got)
	}
}
