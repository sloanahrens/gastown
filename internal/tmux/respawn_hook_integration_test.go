//go:build integration

package tmux

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// requireTestSocket returns a per-test socket name and skips the test if
// tmux is not installed. Each test gets its own socket to prevent interference.
// The socket server is cleaned up when the test finishes.
func requireTestSocket(t *testing.T) string {
	t.Helper()
	requireTmux(t)
	socket := constants.TestSocketName("gt-test-hook")
	// KillServer, not a bare `tmux kill-server`: it unlinks the socket file,
	// which tmux leaves behind when the server exits (gt-20di).
	t.Cleanup(func() {
		_ = NewTmuxWithSocket(socket).KillServer()
	})
	return socket
}

// testSession creates a session on the given socket running a simple command.
func testSession(t *testing.T, socket, session, command string) {
	t.Helper()
	args := []string{"-L", socket, "new-session", "-d", "-s", session, command}
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("failed to create test session %q on socket %q: %v\n%s", session, socket, err, out)
	}
	eventually(t, "session "+session+" to appear", func() bool {
		return exec.Command("tmux", "-L", socket, "has-session", "-t", session).Run() == nil
	})
}

// hookJobRunning reports whether the auto-respawn hook's run-shell job for
// session on socket is running (it sleeps 3s before checking the pane).
func hookJobRunning(socket, session string) bool {
	pattern := "sleep 3 && tmux -L " + socket + " list-panes -t '" + session + "'"
	return exec.Command("pgrep", "-f", pattern).Run() == nil
}

func isPaneDead(socket, session string) bool {
	out, err := exec.Command("tmux", "-L", socket, "list-panes", "-t", session, "-F", "#{pane_dead}").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "1"
}

func getPanePID(t *testing.T, socket, session string) string {
	t.Helper()
	out, err := exec.Command("tmux", "-L", socket, "display-message", "-t", session, "-p", "#{pane_pid}").Output()
	if err != nil {
		t.Fatalf("failed to get pane PID for %q: %v", session, err)
	}
	return strings.TrimSpace(string(out))
}

// TestIntegrationAutoRespawnHook_RespawnWorks is the primary regression test: pane dies,
// hook fires on the correct socket, pane comes back alive.
func TestIntegrationAutoRespawnHook_RespawnWorks(t *testing.T) {
	socket := requireTestSocket(t)
	session := "test-respawn"

	t0 := time.Now()
	logT := func(msg string, args ...any) {
		t.Logf("[+%6.2fs] %s", time.Since(t0).Seconds(), fmt.Sprintf(msg, args...))
	}

	testSession(t, socket, session, "sleep 2")
	defer func() { _ = exec.Command("tmux", "-L", socket, "kill-session", "-t", session).Run() }()
	logT("session created with 'sleep 2'")

	// Log the initial pane state
	logT("initial pane_dead=%v, pane_pid=%s", isPaneDead(socket, session), getPanePIDSafe(socket, session))

	tmx := NewTmuxWithSocket(socket)
	if err := tmx.SetAutoRespawnHook(session); err != nil {
		t.Fatalf("SetAutoRespawnHook: %v", err)
	}
	logT("hook installed")

	// Log the hook configuration (try both session and global hooks)
	if hookOut, err := exec.Command("tmux", "-L", socket, "show-hooks", "-t", session).CombinedOutput(); err == nil {
		logT("session hooks: %s", strings.TrimSpace(string(hookOut)))
	}
	if hookOut, err := exec.Command("tmux", "-L", socket, "show-hooks", "-g").CombinedOutput(); err == nil {
		logT("global hooks: %s", strings.TrimSpace(string(hookOut)))
	}
	// Log the actual remain-on-exit setting
	if optOut, err := exec.Command("tmux", "-L", socket, "show-options", "-t", session, "remain-on-exit").CombinedOutput(); err == nil {
		logT("remain-on-exit: %s", strings.TrimSpace(string(optOut)))
	}
	// Log tmux version (hook behavior varies)
	if verOut, err := exec.Command("tmux", "-V").CombinedOutput(); err == nil {
		logT("tmux version: %s", strings.TrimSpace(string(verOut)))
	}

	eventually(t, "the pane to die (sleep 2 exits)", func() bool { return isPaneDead(socket, session) })
	logT("pane died (sleep 2 exited)")

	// The hook sleeps 3s, sees the pane still dead, and respawns it.
	alive := false
	deadline := time.Now().Add(integrationWait)
	for time.Now().Before(deadline) {
		if !isPaneDead(socket, session) {
			logT("pane respawned (alive again)")
			alive = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !alive {
		// Dump diagnostics on failure
		logT("FAILURE: pane was NOT respawned within %s of death", integrationWait)
		logT("pane_dead=%v, pane_pid=%s", isPaneDead(socket, session), getPanePIDSafe(socket, session))
		if paneInfo, err := exec.Command("tmux", "-L", socket, "list-panes", "-t", session,
			"-F", "dead=#{pane_dead} pid=#{pane_pid} cmd=#{pane_current_command} start=#{pane_start_command}").CombinedOutput(); err == nil {
			logT("pane info: %s", strings.TrimSpace(string(paneInfo)))
		} else {
			logT("list-panes failed: %v", err)
		}
		if hookOut, err := exec.Command("tmux", "-L", socket, "show-hooks", "-t", session).CombinedOutput(); err == nil {
			logT("hooks at failure: %s", strings.TrimSpace(string(hookOut)))
		}
		// Check if remain-on-exit is set (needed for hook to fire)
		if optOut, err := exec.Command("tmux", "-L", socket, "show-options", "-t", session, "remain-on-exit").CombinedOutput(); err == nil {
			logT("remain-on-exit: %s", strings.TrimSpace(string(optOut)))
		}
		t.Error("pane was NOT respawned — hook failed (likely missing -L socket flag)")
	}
}

// getPanePIDSafe returns the pane PID or "?" if it can't be read (no t.Fatal).
func getPanePIDSafe(socket, session string) string {
	out, err := exec.Command("tmux", "-L", socket, "display-message", "-t", session, "-p", "#{pane_pid}").Output()
	if err != nil {
		return "?(err)"
	}
	return strings.TrimSpace(string(out))
}

// TestIntegrationAutoRespawnHook_SkipsAlreadyAlive verifies the dead-pane guard: if the
// daemon restarts the pane during the hook's 3s sleep, the hook must NOT kill
// the fresh process.
func TestIntegrationAutoRespawnHook_SkipsAlreadyAlive(t *testing.T) {
	socket := requireTestSocket(t)
	session := "test-skip-alive"

	testSession(t, socket, session, "sleep 300")
	defer func() { _ = exec.Command("tmux", "-L", socket, "kill-session", "-t", session).Run() }()

	tmx := NewTmuxWithSocket(socket)
	if err := tmx.SetAutoRespawnHook(session); err != nil {
		t.Fatalf("SetAutoRespawnHook: %v", err)
	}

	// Kill the process: the pane dies and the hook's job starts its 3s sleep.
	if out, err := exec.Command("tmux", "-L", socket, "respawn-pane", "-k", "-t", session, "true").CombinedOutput(); err != nil {
		t.Fatalf("respawn-pane true: %v\n%s", err, out)
	}
	eventually(t, "the pane to die", func() bool { return isPaneDead(socket, session) })
	eventually(t, "the hook job to start", func() bool { return hookJobRunning(socket, session) })

	// Simulate the daemon: respawn the pane while the hook is still asleep.
	if out, err := exec.Command("tmux", "-L", socket, "respawn-pane", "-k", "-t", session, "sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("respawn-pane sleep: %v\n%s", err, out)
	}
	eventually(t, "the respawned pane to be alive", func() bool { return !isPaneDead(socket, session) })
	pid1 := getPanePID(t, socket, session)

	// The hook wakes, finds the pane alive, and must leave it alone.
	eventually(t, "the hook job to finish", func() bool { return !hookJobRunning(socket, session) })

	pid2 := getPanePID(t, socket, session)
	if pid1 != pid2 {
		t.Errorf("hook killed daemon-respawned process: PID %s → %s (race condition)", pid1, pid2)
	}
}
