package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// ErrLockHeld means another run holds the town's metrics lock.
var ErrLockHeld = errors.New("town-metrics lock is held by another run")

// heldTownLocks names the lock paths this process holds. A POSIX record lock
// belongs to the process, so it cannot refuse a second acquire from the same
// process; this set does, which keeps the single-flight property inside one
// process as well as across processes.
var heldTownLocks sync.Map // map[string]struct{}

// TownLock is the single-flight lock of one town-metrics run: an exclusive
// POSIX record lock (fcntl F_SETLK) on the whole lock file, plus this process's
// record of holding it.
//
// A POSIX record lock belongs to the process, and a child process does not
// inherit it. flock(2) was the first choice and is wrong here: its lock belongs
// to the open file description, which a forked child holds until it execs, so
// a release could leave the lock held by a child still in flight. With a record
// lock, releasing closes this process's descriptor and nothing else holds the
// lock. The kernel drops it when the holder exits, however it exits, so a run
// the daemon killed leaves nothing behind to take over.
type TownLock struct {
	key string
	f   *os.File
}

// AcquireTownLock takes the lock at path without waiting. It returns
// ErrLockHeld when this process or another process holds it. The file keeps the
// holder's pid for an operator to read; it is never removed, because removing it
// would let a run lock a file no one else is looking at.
func AcquireTownLock(path string) (*TownLock, error) {
	key, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	if _, loaded := heldTownLocks.LoadOrStore(key, struct{}{}); loaded {
		return nil, ErrLockHeld
	}
	lock, err := acquireRecordLock(key)
	if err != nil {
		heldTownLocks.Delete(key)
		return nil, err
	}
	return lock, nil
}

// acquireRecordLock takes the record lock on the file at key and writes the
// holder's pid into it.
func acquireRecordLock(key string) (*TownLock, error) {
	if err := os.MkdirAll(filepath.Dir(key), 0o755); err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	f, err := os.OpenFile(key, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		f.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
			return nil, ErrLockHeld
		}
		return nil, fmt.Errorf("lock %s: %w", key, err)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", key, err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", key, err)
	}
	return &TownLock{key: key, f: f}, nil
}

// Release gives the lock up. Closing the descriptor drops this process's record
// lock on the file, and the process no longer counts itself a holder.
func (l *TownLock) Release() error {
	heldTownLocks.Delete(l.key)
	return l.f.Close()
}
