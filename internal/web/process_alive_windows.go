//go:build windows

package web

import "syscall"

// processAliveOS reports whether a PID is still running, so the Gate panel
// can show a slot whose flock outlived its recorded owner as stale.
func processAliveOS(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = syscall.CloseHandle(handle)
	return true
}

// processQueryLimitedInformation is the least-privileged access right that
// still resolves a live PID (same constant internal/cmd uses).
const processQueryLimitedInformation = 0x1000
