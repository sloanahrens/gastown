//go:build integration

package session

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run sandboxMainScript, the sandbox's main process, against a real
// tmux server. TMUX_TMPDIR gives each test its own server under the script's
// fixed -L socket name, so it cannot touch a real session.

// mainScriptRun is one running sandboxMainScript.
type mainScriptRun struct {
	t    *testing.T
	env  []string
	done chan error
}

func startMainScript(t *testing.T, session string, waitSeconds string) *mainScriptRun {
	t.Helper()
	return startMainScriptWith(t, session, waitSeconds, "", nil)
}

// startMainScriptWith is startMainScript with the unreachable-poll limit set (""
// is the default) and a prepare step that runs against the test's tmux socket
// before the script starts.
func startMainScriptWith(t *testing.T, session string, waitSeconds string, lostPolls string, prepare func(*mainScriptRun)) *mainScriptRun {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	// A unix socket path is limited to about 104 bytes, and t.TempDir under a
	// long test name exceeds it on macOS, so the tmux server gets a short dir.
	tmpdir, err := os.MkdirTemp("/tmp", "gtm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpdir) })
	r := &mainScriptRun{t: t, env: append(os.Environ(), "TMUX_TMPDIR="+tmpdir), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	if prepare != nil {
		prepare(r)
	}
	args := []string{"-c", sandboxMainScript, "sh", session, waitSeconds, "0.2", tmpdir}
	if lostPolls != "" {
		args = append(args, lostPolls)
	}
	cmd := exec.CommandContext(ctx, "sh", args...)
	cmd.Env = r.env
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		kill := exec.Command("tmux", "-L", SandboxTmuxSocket, "kill-server")
		kill.Env = r.env
		_ = kill.Run()
	})
	go func() { r.done <- cmd.Wait() }()
	return r
}

func (r *mainScriptRun) tmux(args ...string) error {
	cmd := exec.Command("tmux", append([]string{"-L", SandboxTmuxSocket}, args...)...)
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Logf("tmux %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return err
}

// waitServer waits for the script to have started its tmux server.
func (r *mainScriptRun) waitServer() {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r.tmux("has-session", "-t", "="+sandboxHolderSession) == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.t.Fatal("the script never started its tmux server")
}

// serverPID is the pid of the tmux server the script started under the test's
// TMUX_TMPDIR.
func (r *mainScriptRun) serverPID() int {
	r.t.Helper()
	cmd := exec.Command("tmux", "-L", SandboxTmuxSocket, "display-message", "-p", "#{pid}")
	cmd.Env = r.env
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("asking the tmux server for its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		r.t.Fatalf("tmux server pid %q: %v", out, err)
	}
	return pid
}

// exited reports the script's exit within timeout; ok is false when it is still
// running.
func (r *mainScriptRun) exited(timeout time.Duration) (err error, ok bool) {
	select {
	case err = <-r.done:
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

func TestIntegrationMainScriptExitsWhenTheAgentSessionIsKilled(t *testing.T) {
	t.Parallel()
	r := startMainScript(t, "gt-agent", "30")
	r.waitServer()
	if err := r.tmux("new-session", "-d", "-s", "gt-agent", "sleep 600"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.exited(1500 * time.Millisecond); ok {
		t.Fatal("the script exited while the agent session was running")
	}
	if err := r.tmux("kill-session", "-t", "=gt-agent"); err != nil {
		t.Fatal(err)
	}
	if err, ok := r.exited(15 * time.Second); !ok || err != nil {
		t.Fatalf("after the session was killed: exited = %v, err = %v; want exit 0", ok, err)
	}
}

func TestIntegrationMainScriptExitsWhenTheAgentProcessEnds(t *testing.T) {
	t.Parallel()
	r := startMainScript(t, "gt-agent", "30")
	r.waitServer()
	if err := r.tmux("new-session", "-d", "-s", "gt-agent", "sleep 2"); err != nil {
		t.Fatal(err)
	}
	if err := r.tmux("set-option", "-w", "-t", "gt-agent", "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	if err, ok := r.exited(15 * time.Second); !ok || err != nil {
		t.Fatalf("after the agent process ended with remain-on-exit: exited = %v, err = %v; want exit 0", ok, err)
	}
}

func TestIntegrationMainScriptSurvivesARespawn(t *testing.T) {
	t.Parallel()
	r := startMainScript(t, "gt-agent", "30")
	r.waitServer()
	if err := r.tmux("new-session", "-d", "-s", "gt-agent", "sleep 600"); err != nil {
		t.Fatal(err)
	}
	if err := r.tmux("set-option", "-w", "-t", "gt-agent", "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	if err := r.tmux("respawn-pane", "-k", "-t", "gt-agent", "sleep 600"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.exited(2500 * time.Millisecond); ok {
		t.Fatal("the script exited across a respawn of the agent's pane")
	}
}

func TestIntegrationMainScriptGivesUpWhenTheSessionNeverAppears(t *testing.T) {
	t.Parallel()
	start := time.Now()
	r := startMainScript(t, "gt-agent", "2")
	r.waitServer()
	if err, ok := r.exited(15 * time.Second); !ok || err == nil {
		t.Fatalf("session never started: exited = %v, err = %v; want a nonzero exit", ok, err)
	}
	if waited := time.Since(start); waited < 1500*time.Millisecond {
		t.Fatalf("the script gave up after %s, before its 2 s wait: it failed early instead of waiting", waited)
	}
}

// A tmux that cannot be asked is an unknown answer, not "the session ended": it
// must not count toward the three that end the sandbox, but a tmux that stays
// unreachable has lost the agent with it, so the script exits after a bounded
// number of unreachable polls. Killing the whole tmux server leaves every tmux
// call failing, as a crashed server would. The agent's session exists before the
// script starts, so it is in its monitor loop from the first poll.
func TestIntegrationMainScriptExitsOnlyAfterTmuxStaysUnreachable(t *testing.T) {
	t.Parallel()
	r := startMainScriptWith(t, "gt-agent", "30", "10", func(r *mainScriptRun) {
		if err := r.tmux("new-session", "-d", "-s", "gt-agent", "sleep 600"); err != nil {
			t.Fatal(err)
		}
	})
	r.waitServer()
	killed := time.Now()
	if err := r.tmux("kill-server"); err != nil {
		t.Fatal(err)
	}
	err, ok := r.exited(10 * time.Second)
	if !ok || err != nil {
		t.Fatalf("with tmux unreachable: exited = %v, err = %v; want exit 0 after the unreachable-poll limit", ok, err)
	}
	// Three positive "gone" polls take 0.6 s; ten unreachable polls take 2 s. An
	// unreachable tmux that counted as gone would have ended it well before 1.2 s.
	if took := time.Since(killed); took < 1200*time.Millisecond {
		t.Fatalf("the script exited %s after tmux became unreachable; unknown polls must not count as the session ending", took)
	}
}

// The test's cleanup must kill the tmux server the script started under the
// test's own TMUX_TMPDIR. A kill-server that reaches the process's default server
// leaves this one running, holding a pty after RemoveAll deletes its socket dir.
func TestIntegrationMainScriptCleanupKillsItsTmuxServer(t *testing.T) {
	t.Parallel()
	var pid int
	t.Run("start", func(t *testing.T) {
		r := startMainScript(t, "gt-agent", "30")
		r.waitServer()
		pid = r.serverPID()
	})
	if pid == 0 {
		t.Fatal("the subtest did not start a tmux server")
	}
	if !waitForExit(pid, 5*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("tmux server %d outlived the test's cleanup: its kill-server did not reach the server under the test's TMUX_TMPDIR", pid)
	}
}

// waitForExit reports whether pid is gone within timeout.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
