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
// process exists under another uid, so it is alive, and so is any other error.
func ProbeAlive(err error) bool {
	return !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone)
}
