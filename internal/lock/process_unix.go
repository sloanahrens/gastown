//go:build !windows

package lock

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/steveyegge/gastown/internal/procid"
)

// setProcessGroup detaches the child into its own process group.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processExists checks if a process with the given PID exists and is alive.
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}

	// On Unix, sending signal 0 checks if process exists without affecting it.
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Signal 0 fails with ESRCH when the process is gone. EPERM means it exists
	// under another uid, so the holder is alive.
	return procid.ProbeAlive(process.Signal(syscall.Signal(0)))
}
