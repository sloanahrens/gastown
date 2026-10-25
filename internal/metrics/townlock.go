package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLockHeld means another run holds the town's metrics lock.
var ErrLockHeld = errors.New("town-metrics lock is held by another run")

// TownLock is the single-flight lock of one town-metrics run: an exclusive
// POSIX record lock (fcntl F_SETLK) on the whole lock file.
//
// A POSIX record lock belongs to the process, and a child process does not
// inherit it. flock(2) was the first choice and is wrong here: its lock belongs
// to the open file description, which a forked child holds until it execs, so
// a release could leave the lock held by a child still in flight. With a record
// lock, releasing closes this process's descriptor and nothing else holds the
// lock. The kernel drops it when the holder exits, however it exits, so a run
// the daemon killed leaves nothing behind to take over.
//
// The lock is per process: a second AcquireTownLock on the same path from the
// same process succeeds. One run takes one lock, and the tests that need a held
// lock hold it from a child process.
type TownLock struct {
	f *os.File
}

// AcquireTownLock takes the lock at path without waiting. It returns
// ErrLockHeld when another process holds it. The file keeps the holder's pid for
// an operator to read; it is never removed, because removing it would let a run
// lock a file no one else is looking at.
func AcquireTownLock(path string) (*TownLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		f.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
			return nil, ErrLockHeld
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &TownLock{f: f}, nil
}

// Release gives the lock up. Closing the descriptor drops this process's record
// lock on the file.
func (l *TownLock) Release() error {
	return l.f.Close()
}
