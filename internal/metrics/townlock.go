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
// flock(2) on the lock file. The kernel drops the lock when the holder exits,
// however it exits, so a run the daemon killed leaves nothing behind to take
// over, and no second run can claim a path between a read and a rename.
type TownLock struct {
	f *os.File
}

// AcquireTownLock takes the lock at path without waiting. It returns
// ErrLockHeld when a live run holds it. The file keeps the holder's pid for an
// operator to read; it is never removed, because removing it would let a run
// lock a file no one else is looking at.
func AcquireTownLock(path string) (*TownLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
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

// Release gives the lock up. Closing the file drops the flock.
func (l *TownLock) Release() error {
	return l.f.Close()
}
