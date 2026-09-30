package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// A provisioned supervisor job the service manager does not know is an error
// even when a daemon is up: nothing restarts that daemon if it dies
// (gt-4k3fj.11).
func TestDaemonCheck_Run_FailsWhenTheProvisionedJobIsNotLoaded(t *testing.T) {
	townRoot := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))

	path, kind := templates.SupervisorFilePath()
	if kind == "" {
		t.Skip("no supervisor on this host")
	}
	body := "<key>WorkingDirectory</key>\n<string>" + townRoot + "</string>"
	if kind == "systemd" {
		body = "WorkingDirectory=" + townRoot + "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := daemonJobState
	t.Cleanup(func() { daemonJobState = prev })
	daemonJobState = func(k string) templates.SupervisorState {
		return templates.SupervisorState{Kind: k, LastExit: -1} // not loaded
	}
	result := NewDaemonCheck().Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusError || !strings.Contains(result.Message, "not loaded") {
		t.Errorf("Run() = (%v, %q), want an error naming the unloaded job", result.Status, result.Message)
	}

	daemonJobState = func(k string) templates.SupervisorState {
		return templates.SupervisorState{Kind: k, Loaded: true, LastExit: -1}
	}
	if result := NewDaemonCheck().Run(&CheckContext{TownRoot: townRoot}); strings.Contains(result.Message, "not loaded") {
		t.Errorf("Run() = %q for a loaded job, want no unloaded-job failure", result.Message)
	}
}

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
