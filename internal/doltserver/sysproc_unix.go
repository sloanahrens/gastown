//go:build !windows

package doltserver

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/steveyegge/gastown/internal/procid"
)

// setProcessGroup puts the command in its own process group so that signals
// sent to the parent process group (e.g. SIGHUP when the caller calls
// syscall.Exec to become tmux) don't reach the spawned process.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processIsAlive checks whether a process with the given PID is still running.
func processIsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return processAliveAfterSignal(process.Signal(syscall.Signal(0)))
}

// processAliveAfterSignal reports whether a signal-0 probe that returned err
// leaves the process alive. EPERM means another uid owns it, so a live server
// is never read as dead.
func processAliveAfterSignal(err error) bool {
	return procid.ProbeAlive(err)
}
