//go:build integration && !windows

package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// TestIntegrationRunSetupCommand_TimeoutTakesTheGroupAndItsGrace guards both bounds the
// setup command inherits from util.SetProcessGroup's Cancel since gt-6t43: the
// timeout ends the whole group the command leads — not the shell alone, which
// would leave a `pnpm install` running against the worktree the rollback is
// deleting — and a group that will not take SIGTERM holds the call for
// ProcessGroupKillGrace before the escalation ends it. setup_command's other
// tests cover a command that fails and one that succeeds; neither ends one.
//
// The deadline is 2s rather than the milliseconds a unit would like, because
// the stub has to reach its trap and background its child before the deadline
// fires: a stubbed command can spend ~200ms on a loaded host just starting.
// The group is left populated and TERM-proof from then on, so the assertion
// that nothing survives is the one that has to hold whatever the host is doing.
//
// Not parallel: it writes util.ProcessGroupKillGrace and SHELL.
func TestIntegrationRunSetupCommand_TimeoutTakesTheGroupAndItsGrace(t *testing.T) {
	mgr, worktree, _, _ := canonicalRig(t)

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// trap '' TERM is inherited across the exec, so nothing in the group
	// answers the polite signal and only the escalation can end it.
	writeWispSetupCommand(t, mgr, "trap '' TERM; sleep 60 & echo $! > "+pidFile+"; wait")

	const timeout = 2 * time.Second
	const grace = 200 * time.Millisecond
	mgr.setupTimeout = timeout
	stubProcessGroupKillGrace(t, grace)
	t.Setenv("SHELL", "/bin/sh")

	start := time.Now()
	err := mgr.runSetupCommand(worktree)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("runSetupCommand = %v, want a timeout error", err)
	}
	if min := timeout + grace; elapsed < min {
		t.Errorf("runSetupCommand returned after %s, want at least %s (the deadline plus its grace): the group was killed without being signaled first",
			elapsed.Round(time.Millisecond), min)
	}
	if elapsed > 30*time.Second {
		t.Errorf("runSetupCommand took %s: the deadline did not bound the call", elapsed.Round(time.Millisecond))
	}
	waitForPidFileGone(t, pidFile)
}

// stubProcessGroupKillGrace shrinks the SIGTERM grace so a test can drive the
// escalation without waiting out the default.
func stubProcessGroupKillGrace(t *testing.T, grace time.Duration) {
	t.Helper()
	prev := util.ProcessGroupKillGrace
	util.ProcessGroupKillGrace = grace
	t.Cleanup(func() { util.ProcessGroupKillGrace = prev })
}

// waitForPidFileGone polls until the process named in pidFile is gone. A
// SIGKILLed process stays visible to kill(pid, 0) as a zombie until it is
// reaped, so "gone" is a state to wait for rather than to sample once.
func waitForPidFileGone(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading the setup command's backgrounded child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parsing pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("child %d survived the setup_command timeout: the process group was not killed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
