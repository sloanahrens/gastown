//go:build windows

package util

import (
	"errors"
	"fmt"
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

// taskkillNotFound is taskkill's exit status for a PID it found no process for.
// It is taskkill's own code rather than a Windows system error: system error
// 128 is ERROR_WAIT_NO_CHILDREN and means something else.
const taskkillNotFound = 128

// taskkillTree runs the tree walk. A var so a test can put taskkill's failure
// statuses in front of KillProcessGroup without provoking them.
var taskkillTree = func(pid int) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// KillProcessGroup ends cmd and everything below it. Windows has no process
// group signal, so this is taskkill's tree walk: the same guarantee a negative
// pid gives on Unix, with no SIGTERM grace to wait out.
//
// A tree that was already gone — the outcome this function exists to produce —
// and a tree taskkill could not reach both leave it with a non-zero status, so
// the status is what decides: taskkillNotFound is the first, and every other
// failure (access denied, taskkill off PATH) means some of the tree may still
// be running and is returned to the caller. Reporting those as success is what
// let a live tree outlive the gate that started it (gt-7hx0).
//
// A root that had already exited before this call puts its descendants beyond
// any pid-keyed walk, this one included: taskkill finds children through their
// parent's pid, and that pid is gone. Unix has no such gap — a process group
// outlives its leader, so a negative pid still reaches the survivors.
//
// cmd must come from exec.CommandContext with SetProcessGroup, for the reason
// SetProcessGroup documents.
func KillProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	err := taskkillTree(pid)
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == taskkillNotFound {
		return nil
	}
	// The walk failed for a reason that is not "already gone", so the root is
	// unaccounted for. Kill it directly: it is the one process reachable
	// without the walk, and leaving it alive would hold the caller's cmd.Wait()
	// on a process nothing else is going to end.
	_ = cmd.Process.Kill()
	return fmt.Errorf("taskkill /T /F /PID %d: %w", pid, err)
}
