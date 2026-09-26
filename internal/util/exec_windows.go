//go:build windows

package util

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// ProcessGroupKillGrace is the SIGTERM-to-SIGKILL grace period on Unix. Windows
// has no signal escalation — KillProcessGroup ends the tree in one step — so
// nothing reads it there; it exists so both build tags offer the same API.
var ProcessGroupKillGrace time.Duration

// SetProcessGroup configures a command to run in its own process group
// without a visible console window, and installs a cancellation hook that ends
// the whole tree at the caller's deadline. On Windows,
// CREATE_NEW_PROCESS_GROUP detaches from the parent's console and
// CREATE_NO_WINDOW suppresses the transient console window that Windows
// otherwise creates for console apps spawned from a no-console parent (e.g.
// the daemon).
//
// cmd must come from exec.CommandContext: the hook rides on Cancel, and
// os/exec refuses to Start a command whose Cancel was set on anything else.
func SetProcessGroup(cmd *exec.Cmd) {
	newProcessGroup(cmd)
	cmd.Cancel = func() error {
		return KillProcessGroup(cmd)
	}
}

// SetDetachedProcessGroup is SetProcessGroup without the cancellation hook.
func SetDetachedProcessGroup(cmd *exec.Cmd) {
	newProcessGroup(cmd)
}

func newProcessGroup(cmd *exec.Cmd) {
	const CREATE_NO_WINDOW = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW,
	}
}

// KillProcessGroup ends cmd and everything below it. Windows has no process
// group signal, so this is taskkill's tree walk: the same guarantee a negative
// pid gives on Unix, with no SIGTERM grace to wait out.
//
// taskkill reports a process tree that is already gone as a failure, the same
// exit status it gives for a tree it failed to reach (e.g. access denied, or
// taskkill missing from PATH), so a taskkill failure alone doesn't tell us the
// tree is dead. On failure we fall back to killing the top-level process
// directly: that either confirms the tree was already gone (os.ErrProcessDone)
// or gives the caller a real error instead of a silent no-op. The fallback
// can't reach grandchildren the way taskkill's /T does, but it beats reporting
// success while the tree is still running.
func KillProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return killErr
		}
	}
	return nil
}
