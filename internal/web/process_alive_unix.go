//go:build !windows

package web

import "syscall"

// processAliveOS reports whether a PID is still running, so the Gate panel
// can show a slot whose flock outlived its recorded owner as stale. signal 0
// performs the existence check without delivering anything.
func processAliveOS(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
