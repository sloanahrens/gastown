package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/util"
)

// TestStrayWorkspaceServerLabels pins the one line `gt dolt status` adds for a
// stray workspace server: it names the pid and the port, names nothing for the
// town's own server, and names nothing for a workspace that is pointed at the
// town's port (gt-4js4f).
func TestStrayWorkspaceServerLabels(t *testing.T) {
	t.Parallel()
	servers := []util.DoltServerProcess{
		{PID: 1395, Port: 3307, ConfigPath: "/Users/me/gt/.dolt-data/config.yaml"},
		{PID: 50095, Port: 50095, ConfigPath: "/Users/me/.claude/.beads/dolt-server-config.yaml"},
		{PID: 51646, Port: 51646, ConfigPath: "/Users/me/.claude/.beads/dolt-server-config.yaml"},
		{PID: 43, Port: 3307, ConfigPath: "/Users/me/proj/.beads/dolt-server-config.yaml"},
		{PID: 44, Port: 0, ConfigPath: "/Users/me/other/.beads/dolt-server-config.yaml"},
	}

	got := strayWorkspaceServerLabels(servers, 3307)
	joined := strings.Join(got, ", ")
	for _, want := range []string{"PID 50095 on port 50095", "PID 51646 on port 51646", "PID 44 on port unknown"} {
		if !strings.Contains(joined, want) {
			t.Errorf("labels missing %q: %v", want, got)
		}
	}
	if strings.Contains(joined, "PID 1395") {
		t.Errorf("labels name the town's own server: %v", got)
	}
	if strings.Contains(joined, "PID 43") {
		t.Errorf("labels name a workspace pointed at the town's port: %v", got)
	}
	if len(got) != 3 {
		t.Errorf("labels = %v, want 3", got)
	}
}
