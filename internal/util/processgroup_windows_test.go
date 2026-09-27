//go:build windows

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

// TestKillProcessGroup_AlreadyGoneIsSuccess pins the one taskkill failure that
// is the outcome this function exists to produce rather than a failure to
// produce it: the process was reaped before the walk ran, and taskkill's status
// for that is taskkillNotFound. Everything KillProcessGroup returns nil for
// rests on this status being the one it is (gt-7hx0), so the test reads it off
// a real taskkill rather than trusting the constant.
func TestKillProcessGroup_AlreadyGoneIsSuccess(t *testing.T) {
	cmd := startInGroup(t, "cmd.exe", "/c", "exit", "0")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait() on a process that exits 0: %v", err)
	}

	if err := KillProcessGroup(cmd); err != nil {
		pid := strconv.Itoa(cmd.Process.Pid)
		direct := exec.Command("taskkill", "/T", "/F", "/PID", pid).Run()
		t.Errorf("KillProcessGroup after the process was reaped = %v, want nil; taskkill on the same pid returns %v", err, direct)
	}
}

// TestKillProcessGroup_SurfacesAFailedTreeWalk guards the fail-open gt-7hx0
// reported: a walk that fails for a reason other than "already gone" leaves
// the tree unaccounted for, so it must reach the caller as an error, and the
// root it could not reach must not be left running behind that error.
func TestKillProcessGroup_SurfacesAFailedTreeWalk(t *testing.T) {
	stubTaskkillTree(t, exitError(t, 1))

	cmd := startInGroup(t, "ping.exe", "-n", "60", "127.0.0.1")

	err := KillProcessGroup(cmd)
	if err == nil {
		t.Fatal("KillProcessGroup with a failing tree walk = nil, want the walk's error")
	}
	if !strings.Contains(err.Error(), "taskkill") {
		t.Errorf("KillProcessGroup error = %v, want it to name the walk that failed", err)
	}
	waitForGone(t, cmd.Process.Pid)
}

// TestKillProcessGroup_TaskkillMissingIsAFailure covers the walk failing
// without an exit status at all, which is the second shape the old code
// swallowed: no taskkill on PATH is a tree nothing reached, not a tree that
// was already down.
func TestKillProcessGroup_TaskkillMissingIsAFailure(t *testing.T) {
	cmd := startInGroup(t, "ping.exe", "-n", "60", "127.0.0.1")
	t.Setenv("PATH", t.TempDir())

	err := KillProcessGroup(cmd)
	if err == nil {
		t.Fatal("KillProcessGroup with no taskkill on PATH = nil, want an error")
	}
	waitForGone(t, cmd.Process.Pid)
}

// TestKillProcessGroup_TakesTheGrandchild guards what the walk is for: ending a
// process below the one exec started, which cmd.Process.Kill() — and so
// exec.CommandContext's default cancel — does not reach. The command here has
// the gate's shape cut to two levels: powershell that starts ping and waits on
// it.
func TestKillProcessGroup_TakesTheGrandchild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := startInGroup(t, powershellPath(t), powershellTreeArgs(pidFile)...)
	grandchild := waitForPID(t, pidFile)

	if err := KillProcessGroup(cmd); err != nil {
		t.Fatalf("KillProcessGroup: %v", err)
	}

	waitForGone(t, grandchild)
	waitForGone(t, cmd.Process.Pid)
}

// TestSetProcessGroup_CancelTakesTheGrandchild guards the wiring every gate in
// the town depends on: a context canceled on a command configured with
// SetProcessGroup must reach the grandchild, not just the shell, and must
// report the cancellation rather than the walk that did not confirm it.
func TestSetProcessGroup_CancelTakesTheGrandchild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := powershellTreeArgs(pidFile)
	cmd := exec.CommandContext(ctx, powershellPath(t), args...)
	SetProcessGroup(cmd)
	// WaitDelay bounds the Wait below: a Cancel that never ends the tree would
	// otherwise hold this test on a process nothing is going to reap.
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatalf("start powershell: %v", err)
	}
	t.Cleanup(func() { _ = KillProcessGroup(cmd) })
	grandchild := waitForPID(t, pidFile)

	cancel()
	if err := cmd.Wait(); err == nil {
		t.Error("cmd.Wait() = nil, want an error for a command canceled at its deadline")
	}

	waitForGone(t, grandchild)
}

// startInGroup starts name with args under SetProcessGroup and returns the
// running command. A caller that needs its exit status calls cmd.Wait().
func startInGroup(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), name, args...)
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		// A failed assertion leaves the tree alive; take it down so the test
		// binary does not hand a stray process to the rest of the suite.
		_ = KillProcessGroup(cmd)
	})
	return cmd
}

// stubTaskkillTree makes the tree walk fail with err for the duration of the
// test. Not parallel-safe: it swaps a package var.
func stubTaskkillTree(t *testing.T, err error) {
	t.Helper()
	prev := taskkillTree
	taskkillTree = func(int) error { return err }
	t.Cleanup(func() { taskkillTree = prev })
}

// exitError returns the error of a process that exited with code, so a test
// can hand the walk a real exit status: exec.ExitError carries nothing a test
// can set, and cmd.exe's own status is the platform's answer rather than a
// guess at one.
func exitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("cmd.exe", "/c", "exit", strconv.Itoa(code)).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
		t.Fatalf("cmd.exe /c exit %d = %v, want an ExitError with status %d", code, err, code)
	}
	return err
}

// powershellPath locates the shell the tree tests run under, skipping the test
// where it is absent rather than reporting a missing tool as a defect.
func powershellPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skipf("powershell.exe is not on PATH: %v", err)
	}
	return path
}

// powershellTreeArgs returns the arguments to powershell running a grandchild
// one level below it, writing that process's pid to pidFile so the test learns
// the pid it must see gone, then waiting on it so the root stays alive.
func powershellTreeArgs(pidFile string) []string {
	script := fmt.Sprintf(
		"$p = Start-Process -PassThru -NoNewWindow -FilePath ping.exe -ArgumentList '-n','60','127.0.0.1'; "+
			"$p.Id | Out-File -FilePath '%s' -Encoding ascii; "+
			"Wait-Process -Id $p.Id",
		pidFile)
	return []string{"-NoProfile", "-NonInteractive", "-Command", script}
}

// processAlive reports whether pid names a running process. OpenProcess fails
// for a pid with no process object behind it, and a terminated process whose
// object is still open answers the wait instead, so the two are read together
// rather than inferred from one.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	event, err := syscall.WaitForSingleObject(h, 0)
	return err == nil && event == syscall.WAIT_TIMEOUT
}

// waitForGone polls until pid is gone: a process killed with TerminateProcess
// stays visible until it has been reaped, so "gone" is a state to wait for
// rather than to sample once.
func waitForGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if !time.Now().Before(deadline) {
			t.Fatalf("process %d is still running 10s after it should have been killed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid was written to %s", pidFile)
	return 0
}
