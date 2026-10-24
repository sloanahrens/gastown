//go:build !windows

package cmd

import (
	"syscall"

	"github.com/steveyegge/gastown/internal/procid"
)

// isProcessRunning checks if a process with the given PID exists. EPERM counts
// as running: the process exists under another uid.
func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	return procid.ProbeAlive(syscall.Kill(pid, 0))
}
