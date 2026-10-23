//go:build integration

package session

import (
	"context"
	"os"
	"os/exec"
	"strings"
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
	cmd := exec.CommandContext(ctx, "sh", "-c", sandboxMainScript, "sh", session, waitSeconds, "0.2", tmpdir)
	cmd.Env = r.env
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = exec.Command("tmux", "-L", SandboxTmuxSocket, "kill-server").Run()
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
