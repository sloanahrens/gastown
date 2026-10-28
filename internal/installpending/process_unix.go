//go:build !windows

package installpending

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a running process: kill(pid, 0)
// answers ESRCH only for a pid with no process. EPERM is a process this user
// may not signal, and every other error fails safe to alive, because a live
// writer read as dead would let a sling start beside the install (gt-vc34c).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
