//go:build unix

package bird

import (
	"errors"
	"fmt"
	"syscall"
)

// probeProcess reports whether the kernel still has a process for pid. It is
// the second half of startToken, asked only when the token could not be read:
// the answer tells a process that is gone from a table this process may not
// read.
func probeProcess(pid int) (bool, error) {
	return probeAnswer(syscall.Kill(pid, 0))
}

// probeAnswer reads a signal-0 probe. Only "no such process" is death: an
// answer the kernel gives for another reason leaves the process unjudged, and
// a caller must not prune a bird on that.
func probeAnswer(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		// It exists under a uid we are not: alive, and its token stays unread.
		return true, nil
	default:
		return false, fmt.Errorf("probing process liveness: %w", err)
	}
}
