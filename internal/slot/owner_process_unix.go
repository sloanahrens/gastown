//go:build !windows

package slot

import (
	"errors"
	"syscall"
)

// processGone is true only when kill(pid, 0) reports ESRCH: no such process.
// EPERM means a process exists that this user may not signal, so it is not
// gone.
func processGone(pid int) bool {
	if pid <= 0 {
		return false
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
