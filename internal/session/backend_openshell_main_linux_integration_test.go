//go:build integration && linux

package session

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/procid"
)

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
