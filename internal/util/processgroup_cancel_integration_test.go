//go:build integration && !windows

package util

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestIntegrationKillProcessGroup_AFinishedCommandReportsDone pins the
// contract exec.Cmd.Cancel reads: a group that was already empty is reported
// as os.ErrProcessDone, not as a kill that interrupted something (gt-7npde).
func TestIntegrationKillProcessGroup_AFinishedCommandReportsDone(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sh", "-c", "exit 0")
	SetDetachedProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	// cmd.Run reaped the command, so its group has no members left.
	if err := KillProcessGroup(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("KillProcessGroup on a command that already finished = %v, want os.ErrProcessDone", err)
	}
}

// TestIntegrationSetProcessGroup_SuccessAtTheContextEndStaysASuccess guards
// gt-7npde end to end: a command that exits 0 in the same instant its context
// ends keeps that success, rather than being recorded as the context's error
// (a spec dispatch tick that finished at its 5-minute bound logged "tick
// failed" and wrote no tick record).
//
// The daemon reaches this by luck — its command exits while exec's Wait is
// reaping it, so the group is empty by the time Cancel signals it — and a
// test that waited for the same luck would be flaky. The interleaving is
// forced instead: Cancel is the group signal, and the deferred one here blocks
// until kill(pid, 0) reports the reap that empties the group.
func TestIntegrationSetProcessGroup_SuccessAtTheContextEndStaysASuccess(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "exit 0")
	SetProcessGroup(cmd)

	killGroup := cmd.Cancel
	reaped := make(chan struct{})
	cmd.Cancel = func() error {
		<-reaped
		return killGroup()
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()

	// Wait reaps the command, and an unreaped child is still a member of its
	// group: kill(pid, 0) says ESRCH only once the group is empty.
	go func() {
		defer close(reaped)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	cancel()

	select {
	case err := <-wait:
		if err != nil {
			t.Errorf("cmd.Wait() = %v, want nil: the command exited 0 as its context ended, so Cancel reported the empty group wrongly and exec replaced the success with the context's error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cmd.Wait() did not return: Cancel is waiting on a reap that never came")
	}
}
