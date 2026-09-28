//go:build integration && !windows

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// TestIntegrationKillSessionWithProcesses_DoesNotKillUnrelatedProcesses runs
// the kill against the real process table and ps: an unrelated process must
// survive a session kill.
func TestIntegrationKillSessionWithProcesses_DoesNotKillUnrelatedProcesses(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-nounrelated-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session with a long-running process
	if err := tm.NewSessionWithCommand(sessionName, "", "sleep 300"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Start a separate background process (simulating an unrelated process)
	// This process runs in its own process group (via setsid or just being separate)
	sentinel := exec.Command("sleep", "300")
	if err := sentinel.Start(); err != nil {
		t.Fatalf("starting sentinel process: %v", err)
	}
	sentinelPID := sentinel.Process.Pid
	defer func() { _ = sentinel.Process.Kill(); _ = sentinel.Wait() }()

	eventually(t, "the pane to run sleep", func() bool {
		cmd, _ := tm.GetPaneCommand(sessionName)
		return cmd == "sleep"
	})

	// Kill session with processes
	if err := tm.KillSessionWithProcesses(sessionName); err != nil {
		t.Fatalf("KillSessionWithProcesses: %v", err)
	}

	// The sentinel process should still be alive (it's unrelated)
	// Check by sending signal 0 (existence check)
	if err := sentinel.Process.Signal(os.Signal(nil)); err != nil {
		// Process.Signal(nil) isn't reliable on all platforms, use kill -0
		checkCmd := exec.Command("kill", "-0", fmt.Sprintf("%d", sentinelPID))
		if checkErr := checkCmd.Run(); checkErr != nil {
			t.Errorf("sentinel process %d was killed (should have survived since it's unrelated)", sentinelPID)
		}
	}
}
