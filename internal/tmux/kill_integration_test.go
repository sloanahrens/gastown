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

// TestIntegrationKillSession_NoPrefixMatch: tmux resolves a session target by
// prefix when no session carries the name outright, so once "kx" has died
// KillSession("kx") matched "kx-b" and killed that instead. The clean-up kill
// of a session that is already gone must leave its longer-named neighbor alone
// (gt-u4lr9).
func TestIntegrationKillSession_NoPrefixMatch(t *testing.T) {
	tm := newTestTmux(t)
	name, sibling := "gt-test-kx", "gt-test-kx-b"
	createSessionPair(t, tm, name, sibling)

	// Let the exact-named session die on its own: its pane process ends and,
	// with remain-on-exit off, tmux destroys the session with it.
	if err := tm.KillPaneProcesses(name); err != nil {
		t.Fatalf("KillPaneProcesses(%s): %v", name, err)
	}
	eventually(t, name+" to be gone", func() bool { return !sessionAlive(t, tm, name) })

	if err := tm.KillSession(name); err != nil {
		t.Fatalf("KillSession(%s) = %v, want nil for a session that is already gone", name, err)
	}
	if !sessionAlive(t, tm, sibling) {
		t.Errorf("KillSession(%s) killed its neighbor %s", name, sibling)
	}
}

// TestIntegrationKillSessionWithProcesses_NoPrefixMatch is the same prefix
// collision on the process-killing path, where it costs more than the session:
// the pane PID lookup used to resolve the dead "kx" to the live "kx-b", so the
// kill terminated that session's process tree before killing the session
// itself.
func TestIntegrationKillSessionWithProcesses_NoPrefixMatch(t *testing.T) {
	tm := newTestTmux(t)
	name, sibling := "gt-test-kx-proc", "gt-test-kx-proc-b"
	createSessionPair(t, tm, name, sibling)

	if err := tm.KillPaneProcesses(name); err != nil {
		t.Fatalf("KillPaneProcesses(%s): %v", name, err)
	}
	eventually(t, name+" to be gone", func() bool { return !sessionAlive(t, tm, name) })

	if err := tm.KillSessionWithProcesses(name); err != nil {
		t.Fatalf("KillSessionWithProcesses(%s) = %v, want nil for a session that is already gone", name, err)
	}
	if !sessionAlive(t, tm, sibling) {
		t.Errorf("KillSessionWithProcesses(%s) killed its neighbor %s", name, sibling)
	}
}

// createSessionPair starts two sessions, the second named name + "-b", each
// running a process that outlives the test, and kills both on cleanup.
func createSessionPair(t *testing.T, tm *Tmux, name, sibling string) {
	t.Helper()
	for _, s := range []string{name, sibling} {
		if err := tm.NewSessionWithCommand(s, "", "sleep 300"); err != nil {
			t.Fatalf("NewSessionWithCommand(%s): %v", s, err)
		}
		t.Cleanup(func() { _ = tm.KillSession(s) })
	}
}

// sessionAlive reports whether the session exists, failing the test on any
// error other than "no such session".
func sessionAlive(t *testing.T, tm *Tmux, name string) bool {
	t.Helper()
	running, err := tm.HasSession(name)
	if err != nil {
		t.Fatalf("HasSession(%s): %v", name, err)
	}
	return running
}
