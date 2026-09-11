package doctor

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDaemonCheck_Run_SurfacesSupervisedStatus verifies the daemon check's
// details include a "Supervised: <value>" line, matching what
// 'gt daemon status' reports, whether or not the daemon is running.
func TestDaemonCheck_Run_SurfacesSupervisedStatus(t *testing.T) {
	townRoot := t.TempDir()

	// Isolate HOME/XDG_DATA_HOME so SupervisorStatus() sees no plist/unit and
	// this test is deterministic regardless of the host machine's state.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))

	check := NewDaemonCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	found := false
	for _, d := range result.Details {
		if strings.HasPrefix(d, "Supervised: ") {
			found = true
			if d != "Supervised: none" {
				t.Errorf("Details supervised line = %q, want %q", d, "Supervised: none")
			}
		}
	}
	if !found {
		t.Errorf("DaemonCheck.Run() details = %v, want a 'Supervised: ' line", result.Details)
	}
}
