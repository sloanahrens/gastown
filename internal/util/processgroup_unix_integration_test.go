//go:build integration && !windows

package util

import (
	"bufio"
	"context"
	"errors"
	"io"
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
func TestIntegrationKillProcessGroup_TakesTheGrandchild(t *testing.T) {
	t.Parallel()

	cmd, done, grandchild := startInGroup(t, "sleep 60 & echo $!; wait")

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
func TestIntegrationKillProcessGroup_EscalatesPastSIGTERM(t *testing.T) {
	// Not parallel: this test writes ProcessGroupKillGrace, and parallel
	// siblings would race it.
	stubProcessGroupKillGrace(t, 300*time.Millisecond)

	// The shell ignores SIGTERM, and its children inherit that disposition, so
	// nothing in the group is reachable by a polite signal.
	cmd, done, grandchild := startInGroup(t, `trap "" TERM; sleep 60 & echo $!; wait`)

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
// the town depends on: the end of a command's context (a deadline, in the
// gates) on a command configured with SetProcessGroup must reach the
// grandchild, not just the shell.
//
// The test ends the context itself, once the grandchild's pid is on disk. It
// used to give the context a 300 ms deadline, which a loaded host could spend
// before the shell even wrote the pid, failing with "no pid was written"
// (a 1 ms deadline reproduces that every time). cmd.Cancel runs the same way
// whichever ended the context.
func TestIntegrationSetProcessGroup_CancelTakesTheGrandchild(t *testing.T) {
	// Not parallel: this test writes ProcessGroupKillGrace, and parallel
	// siblings would race it.
	stubProcessGroupKillGrace(t, 300*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 60 & echo $!; wait")
	SetProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = KillProcessGroup(cmd) })
	grandchild := readPID(t, stdout)

	cancel()
	if err := cmd.Wait(); err == nil {
		t.Error("cmd.Wait() = nil, want the error of a command killed when its context ended")
	}

	waitForGone(t, grandchild)
}

// TestSetProcessGroup_CancelSendsSIGTERMFirst guards the half of gt-6t43's
// shared change that a group dying on either signal does not distinguish: the
// group is signalled politely before anything is forced on it. The trap writes
// a marker, so a Cancel that reached for SIGKILL — what every caller had
// before gt-6t43 — leaves the marker unwritten and fails here.
//
// It runs against the shipped ProcessGroupKillGrace rather than a stub: the
// trap has to win the window this test leaves it, and a default too short to
// hold one is the regression worth failing on.
func TestIntegrationSetProcessGroup_CancelSendsSIGTERMFirst(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "reaped")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// $0 is the marker path (sh -c script name). The group waits on a
	// backgrounded sleep, so it survives until something signals it. The
	// subshell resets SIGTERM to its default: a shell with a TERM trap hands
	// that disposition to what it backgrounds, so otherwise the sleep would
	// outlast the polite signal and the escalation would end the group
	// instead — reachable, but not what this test is measuring.
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap 'touch "$0"; exit 0' TERM; (trap - TERM; sleep 60) & echo $!; wait`, marker)
	SetProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = KillProcessGroup(cmd) })
	grandchild := readPID(t, stdout)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("cmd.Wait() = %v, want context.Canceled from the cancelled context", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the group's TERM trap did not run: %v — Cancel skipped SIGTERM", err)
	}
	waitForGone(t, grandchild)
	waitForGroupGone(t, cmd.Process.Pid)
}

// startInGroup starts sh with script under SetProcessGroup and returns the
// running command, a channel carrying its Wait error, and the pid the script
// printed on its first line of stdout (the grandchild's). The context is
// background — SetProcessGroup requires a CommandContext command, and these
// tests drive the kill themselves.
func startInGroup(t *testing.T, script string) (*exec.Cmd, <-chan error, int) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
	SetProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sh -c %q: %v", script, err)
	}
	t.Cleanup(func() {
		// A failed assertion leaves the group alive; take it down so the test
		// binary does not hand a stray `sleep 60` to the rest of the suite.
		_ = KillProcessGroup(cmd)
	})
	pid := readPID(t, stdout)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, done, pid
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

// readPID reads the pid a script printed as its first line of stdout. The
// read blocks until the script has written it, or fails at EOF if the script
// died first; it never races a deadline. (It used to poll a pid file for 5 s,
// which a loaded host could spend before sh started.)
func readPID(t *testing.T, stdout io.Reader) int {
	t.Helper()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the grandchild's pid: %q, %v", line, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("grandchild pid %q: %v", line, err)
	}
	return pid
}
