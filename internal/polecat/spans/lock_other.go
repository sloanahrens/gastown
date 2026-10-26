//go:build !unix

package spans

import (
	"errors"
	"os"
)

// logLock is the append lock of one bead's span log. On a platform without the
// POSIX record lock there is no lock to take, so an append fails rather than
// writing a log it cannot keep whole.
type logLock struct{}

var errNoSpanLock = errors.New("polecat spans need a unix platform for the append lock")

// lockLog reports that the append lock is unavailable on this platform.
func lockLog(path string) (*logLock, error) {
	return nil, errNoSpanLock
}

// unlock is never reached: lockLog returns an error on this platform.
func (l *logLock) unlock() {}

// openLogNoBlock is os.Open where the non-blocking open is not available.
func openLogNoBlock(path string) (*os.File, error) {
	return os.Open(path)
}
