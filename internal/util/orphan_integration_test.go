//go:build integration && !windows

package util

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tmux"
)

// Against the real process table, lsof and tmux: the probes the unit tier's
// orphan logic reads through.

// TestGetProcessCwd checks getProcessCwd against a directory the test chooses,
// not the ambient checkout. The kernel (lsof, /proc/<pid>/cwd) reports the
// physical path, while os.Getwd returns $PWD's logical spelling when it names
// the same directory. Comparing the two raw failed whenever the checkout was
// reached through a symlink (/tmp -> /private/tmp on macOS). t.TempDir sits
// under such a symlink on macOS (/var -> /private/var), so this exercises it
// on every run instead of depending on where the tree was checked out.
func TestIntegrationGetProcessCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	cwd := getProcessCwd(os.Getpid())
	if cwd == "" {
		t.Fatal("getProcessCwd(self) returned empty string")
	}
	if got, want := realPath(t, cwd), realPath(t, dir); got != want {
		t.Errorf("getProcessCwd(self) = %q (resolved %q), want the directory %q (resolved %q)", cwd, got, dir, want)
	}
}

// hasTmux returns true if tmux is available on PATH.
func hasTmux() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// tmuxSocketSession creates a session on the given socket and returns the pane PID.
// The caller must kill the session or server in cleanup.
func tmuxSocketSession(t *testing.T, socketName, sessionName string) int {
	t.Helper()
	err := exec.Command("tmux", "-L", socketName, "new-session", "-d",
		"-s", sessionName, "-x", "80", "-y", "24", "sleep", "300").Run()
	if err != nil {
		t.Fatalf("create session %q on socket %q: %v", sessionName, socketName, err)
	}

	out, err := exec.Command("tmux", "-L", socketName,
		"list-panes", "-t", sessionName, "-F", "#{pane_pid}").Output()
	if err != nil {
		t.Fatalf("list-panes for %q on %q: %v", sessionName, socketName, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse pane PID %q: %v", string(out), err)
	}
	return pid
}

// killTmuxServer kills a tmux server by socket name. It goes through
// tmux.KillServer rather than a bare `tmux kill-server` because that also
// unlinks the socket file, which tmux leaves behind when its server exits
// (gt-20di).
func killTmuxServer(socketName string) {
	_ = tmux.NewTmuxWithSocket(socketName).KillServer()
}

func TestIntegrationGetTmuxSessionPIDs_CrossSocket(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	socketA := fmt.Sprintf("gt-test-orphan-a-%d", os.Getpid())
	socketB := fmt.Sprintf("gt-test-orphan-b-%d", os.Getpid())
	t.Cleanup(func() {
		killTmuxServer(socketA)
		killTmuxServer(socketB)
	})

	pidA := tmuxSocketSession(t, socketA, "session-a")
	pidB := tmuxSocketSession(t, socketB, "session-b")

	// Set default socket to A — simulates being inside Town A's context
	oldSocket := tmux.GetDefaultSocket()
	tmux.SetDefaultSocket(socketA)
	t.Cleanup(func() { tmux.SetDefaultSocket(oldSocket) })

	pids := getTmuxSessionPIDs()

	if !pids[pidA] {
		t.Errorf("PID %d from socket A not in protected set (set size=%d)", pidA, len(pids))
	}
	if !pids[pidB] {
		t.Errorf("PID %d from socket B not in protected set — cross-town process would be killed (set size=%d)", pidB, len(pids))
	}
}

func TestIntegrationGetTmuxSessionPIDs_SingleSocket(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	socket := fmt.Sprintf("gt-test-orphan-single-%d", os.Getpid())
	t.Cleanup(func() { killTmuxServer(socket) })

	pid1 := tmuxSocketSession(t, socket, "session-1")
	pid2 := tmuxSocketSession(t, socket, "session-2")

	oldSocket := tmux.GetDefaultSocket()
	tmux.SetDefaultSocket(socket)
	t.Cleanup(func() { tmux.SetDefaultSocket(oldSocket) })

	pids := getTmuxSessionPIDs()

	if !pids[pid1] {
		t.Errorf("PID %d from session-1 not in protected set", pid1)
	}
	if !pids[pid2] {
		t.Errorf("PID %d from session-2 not in protected set", pid2)
	}
}

// realPath resolves symlinks in a path. On macOS, /var -> /private/var,
// and lsof returns the real path while t.TempDir() returns the symlinked one.
func realPath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return resolved
}
