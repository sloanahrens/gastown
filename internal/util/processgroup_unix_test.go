//go:build !windows

package util

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestKillProcessGroup_TakesTheGrandchild guards gt-6t43's shared helper: the
// whole point of it is reaching a process that is below the one exec started,
// which cmd.Process.Kill() — and so exec.CommandContext's default cancel — does
// not. The command here has the gate's shape cut to two levels: a shell that
// backgrounds a long-lived grandchild and waits on it.
func TestKillProcessGroup_TakesTheGrandchild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	pgrep := fmt.Sprintf("sleep 60 & echo $! > %q; wait", pidFile)

	cmd, done := startInGroup(t, pgrep)
	grandchild := waitForPID(t, pidFile)

	if err := KillProcessGroup(cmd); err != nil {
		t.Fatalf("KillProcessGroup: %v", err)
	}
	if err := <-done; err == nil {
		t.Error("cmd.Wait() = nil, want the error of a killed command")
	}

	waitForGone(t, grandchild)
	waitForGroupGone(t, cmd.Process.Pid)
}

// TestKillProcessGroup_EscalatesPastSIGTERM guards the grace period's other
// half: a group that ignores SIGTERM must still be gone when KillProcessGroup
// returns, and reaching it must have cost the escalation rather than leaving
// the group standing behind a polite signal.
func TestKillProcessGroup_EscalatesPastSIGTERM(t *testing.T) {
	// Not parallel: this test writes ProcessGroupKillGrace, and parallel
	// siblings would race it.
	stubProcessGroupKillGrace(t, 300*time.Millisecond)

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	// The shell ignores SIGTERM, and its children inherit that disposition, so
	// nothing in the group is reachable by a polite signal.
	stubborn := fmt.Sprintf(`trap "" TERM; sleep 60 & echo $! > %q; wait`, pidFile)

	cmd, done := startInGroup(t, stubborn)
	grandchild := waitForPID(t, pidFile)

	start := time.Now()
	if err := KillProcessGroup(cmd); err != nil {
		t.Fatalf("KillProcessGroup: %v", err)
	}
	elapsed := time.Since(start)
	if err := <-done; err == nil {
		t.Error("cmd.Wait() = nil, want the error of a killed command")
	}

	if elapsed < 300*time.Millisecond {
		t.Errorf("KillProcessGroup returned after %s, before the grace period it waited out", elapsed.Round(time.Millisecond))
	}
	waitForGone(t, grandchild)
	waitForGroupGone(t, cmd.Process.Pid)
}

// TestSetProcessGroup_CancelTakesTheGrandchild guards the wiring every gate in
// the town depends on: a context deadline on a command configured with
// SetProcessGroup must reach the grandchild, not just the shell.
func TestSetProcessGroup_CancelTakesTheGrandchild(t *testing.T) {
	// Not parallel: this test writes ProcessGroupKillGrace, and parallel
	// siblings would race it.
	stubProcessGroupKillGrace(t, 300*time.Millisecond)

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	pgrep := fmt.Sprintf("sleep 60 & echo $! > %q; wait", pidFile)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", pgrep)
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = KillProcessGroup(cmd) })
	grandchild := waitForPID(t, pidFile)

	if err := cmd.Wait(); err == nil {
		t.Error("cmd.Wait() = nil, want the error of a command killed at its deadline")
	}

	waitForGone(t, grandchild)
}

// startInGroup starts sh with script under SetProcessGroup and returns the
// running command plus a channel carrying its Wait error. The context is
// background — SetProcessGroup requires a CommandContext command, and these
// tests drive the kill themselves.
func startInGroup(t *testing.T, script string) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sh -c %q: %v", script, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		// A failed assertion leaves the group alive; take it down so the test
		// binary does not hand a stray `sleep 60` to the rest of the suite.
		_ = KillProcessGroup(cmd)
	})
	return cmd, done
}

// stubProcessGroupKillGrace shrinks the SIGTERM grace so a test can drive the
// escalation without waiting out the default.
func stubProcessGroupKillGrace(t *testing.T, grace time.Duration) {
	t.Helper()
	prev := ProcessGroupKillGrace
	ProcessGroupKillGrace = grace
	t.Cleanup(func() { ProcessGroupKillGrace = prev })
}

// waitForGone polls until pid is gone. A process killed with SIGKILL stays
// visible to kill(pid, 0) as a zombie until its parent reaps it, and a member
// of a killed group is reparented before that, so "gone" is a state to wait for
// rather than to sample once.
func waitForGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("process %d is still there after 5s: kill(pid, 0) = %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForGroupGone is waitForGone for a whole process group.
func waitForGroupGone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("process group %d still has members after 5s: kill(-pgid, 0) = %v", pgid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no pid was written to %s", pidFile)
	return 0
}
