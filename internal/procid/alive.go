package procid

import (
	"errors"
	"os"
	"syscall"
)

// ProbeAlive reports whether a signal-0 probe that returned err leaves the
// process alive. Only ESRCH means it is gone. os.Process.Signal reports ESRCH
// as os.ErrProcessDone, and so does a child this process already reaped, whose
// pid may now belong to another process; both count as gone. EPERM means the
// process exists under another uid, so it is alive.
func ProbeAlive(err error) bool {
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return false
	}
	// Every other error, EPERM included, fails safe to alive: a live holder
	// must never be read as dead, so an error we do not recognize keeps it.
	return true
}
