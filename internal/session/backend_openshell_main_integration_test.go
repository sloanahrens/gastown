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

	"github.com/steveyegge/gastown/internal/procid"
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

// wait blocks until the script exits. It carries no duration of its own: a script
// that never exits ends the test with the go test timeout, and a loaded host
// cannot fail it by the clock.
func (r *mainScriptRun) wait() error {
	return <-r.done
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
	if err := r.wait(); err != nil {
		t.Fatalf("after the session was killed: err = %v; want exit 0", err)
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
	if err := r.wait(); err != nil {
		t.Fatalf("after the agent process ended with remain-on-exit: err = %v; want exit 0", err)
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
	if err := r.wait(); err == nil {
		t.Fatal("session never started: exit = nil; want a nonzero exit")
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
	if err := r.wait(); err != nil {
		t.Fatalf("with tmux unreachable: err = %v; want exit 0 after the unreachable-poll limit", err)
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
	var server procid.ID
	t.Run("start", func(t *testing.T) {
		r := startMainScript(t, "gt-agent", "30")
		r.waitServer()
		var ok bool
		if server, ok = procid.Of(r.serverPID()); !ok {
			t.Fatal("reading the tmux server's start time")
		}
	})
	if server.PID == 0 {
		t.Fatal("the subtest did not start a tmux server")
	}
	// The cleanup runs as the subtest returns, so the server is on its way out
	// by now. Judge that by the recorded start token, not the bare pid: the
	// kernel may hand the number to another process, which a signal-0 probe
	// cannot tell from the server.
	if !waitForGone(server, 5*time.Second) {
		_ = killIfSame(server, syscall.SIGKILL)
		t.Fatalf("tmux server %d outlived the test's cleanup: its kill-server did not reach the server under the test's TMUX_TMPDIR", server.PID)
	}
}

// killIfSame sends sig to server's process only when the recorded start token
// still names it, and reports whether a signal was sent. The pid alone may
// belong to a stranger by now, which must never be signalled.
func killIfSame(server procid.ID, sig syscall.Signal) bool {
	if !server.Running(procid.StartToken) {
		return false
	}
	return syscall.Kill(server.PID, sig) == nil
}

// TestIntegrationMainScriptCleanupSparesAStrangerOnTheServerPID drives the
// fallback kill's refusal branch: when the recorded token no longer names the
// pid's process, no signal may go out.
func TestIntegrationMainScriptCleanupSparesAStrangerOnTheServerPID(t *testing.T) {
	t.Parallel()
	self, ok := procid.Of(os.Getpid())
	if !ok {
		t.Fatal("reading the test process's start time")
	}
	// SIGCONT stands in for SIGKILL: the guard decides before the signal, and
	// a delivery to this live process is harmless should the guard regress.
	reused := procid.ID{PID: self.PID, Start: self.Start + "-other"}
	if killIfSame(reused, syscall.SIGCONT) {
		t.Fatal("the fallback kill signalled a pid the recorded token does not name")
	}
}

// TestIntegrationMainScriptGoneCheckIgnoresAReusedPID stands in for the process
// that may take a dead server's pid: the test process is alive, and only its own
// start token names it, so a record carrying another token reads as gone.
func TestIntegrationMainScriptGoneCheckIgnoresAReusedPID(t *testing.T) {
	t.Parallel()
	self, ok := procid.Of(os.Getpid())
	if !ok || !self.Running(procid.StartToken) {
		t.Fatal("the test process's start time is not readable, so the checks below would pass vacuously")
	}
	// The probe the check used before it read tokens reports the pid live
	// whether the recorded process or a stranger holds it, so on its own it
	// reads a recycled pid as the server still running. Both directions are
	// asserted below, which is what the probe cannot do.
	if err := syscall.Kill(self.PID, 0); err != nil {
		t.Fatalf("a signal-0 probe of live pid %d failed (%v), so the case below tests nothing", self.PID, err)
	}
	if gone(self, procid.StartToken) {
		t.Fatal("the test process's own record was counted as gone")
	}
	reused := procid.ID{PID: self.PID, Start: self.Start + "-other"}
	if !gone(reused, procid.StartToken) {
		t.Fatal("a reused pid with a different start time was not counted as gone")
	}
	if !waitForGone(reused, time.Second) {
		t.Fatal("the cleanup test's wait counted a reused pid as the server still running")
	}
}

// TestIntegrationMainScriptGoneCheckDoesNotTrustAnUnreadableToken drives the
// refusal branch: an alive pid whose start token cannot be read is not evidence
// that the server died, so it must not read as gone.
func TestIntegrationMainScriptGoneCheckDoesNotTrustAnUnreadableToken(t *testing.T) {
	t.Parallel()
	self, ok := procid.Of(os.Getpid())
	if !ok {
		t.Fatal("reading the test process's start time")
	}
	unreadable := func(int) (string, bool) { return "", false }
	if gone(self, unreadable) {
		t.Fatal("an unreadable start token was counted as the process gone")
	}
}

// waitForGone reports whether server is gone within timeout. A pid alone is not
// an identity: once its process dies the number can be reused, so a recorded
// start token that no longer matches reads as gone.
func waitForGone(server procid.ID, timeout time.Duration) bool {
	return waitForExit(func() bool { return gone(server, procid.StartToken) }, timeout, time.Now, time.Sleep)
}

// gone reports whether server's process is gone, judging the start token through
// start (StartToken outside tests). Only evidence that the pid no longer holds
// the recorded process counts: the kernel saying the pid is free, or a readable
// token naming a different process. A token that cannot be read is not proof of
// death, so it keeps the process alive.
func gone(server procid.ID, start func(int) (string, bool)) bool {
	if !procid.ProbeAlive(syscall.Kill(server.PID, 0)) {
		return true
	}
	token, ok := start(server.PID)
	return ok && token != server.Start
}
