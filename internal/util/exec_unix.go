//go:build !windows

package util

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// ProcessGroupKillGrace bounds how long KillProcessGroup waits for a process
// group to exit on SIGTERM before escalating to SIGKILL. A var rather than a
// const so a test can drive the escalation without sleeping out the default.
var ProcessGroupKillGrace = 2 * time.Second

// processGroupKillPoll is how often KillProcessGroup re-checks whether the
// group is empty while it waits out ProcessGroupKillGrace.
const processGroupKillPoll = 20 * time.Millisecond

// SetProcessGroup configures a command to run in its own process group so that
// context cancellation kills the entire process tree, preventing orphaned
// children.
//
// cmd must come from exec.CommandContext: the hook rides on Cancel, and os/exec
// refuses to Start a command whose Cancel was set on anything else.
func SetProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return KillProcessGroup(cmd)
	}
}

// SetDetachedProcessGroup configures a command to run in its own process
// group without installing a cancellation hook.
func SetDetachedProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillProcessGroup terminates every process in cmd's process group: SIGTERM to
// the group so it can shut its own children down, then SIGKILL to whatever is
// still there after ProcessGroupKillGrace. cmd.Process.Kill() — the child exec
// started, and so everything exec.CommandContext's default cancel reaches —
// leaves a shell's `make test` and test binaries running past the gate that
// started them (gt-6t43); a negative pid is what reaches them.
//
// cmd must have been started with SetProcessGroup or SetDetachedProcessGroup:
// without Setpgid its children share the caller's process group, and this
// would signal the caller's own.
func KillProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return killProcessGroup(cmd.Process.Pid)
}

func killProcessGroup(pgid int) error {
	// A group that is already empty is the outcome this function exists to
	// produce, not a failure to produce it.
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	deadline := time.Now().Add(ProcessGroupKillGrace)
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return nil // every member is gone
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(processGroupKillPoll)
	}

	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
