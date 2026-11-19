package bdgateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The workspace lock is the one exclusive advisory lock a gateway process holds
// while it changes the live tree: a push (Put) and the recovery that opens the
// store. It is an flock on `<parent>/.<base>.lock`, beside the root, so two
// gateway processes on one volume take turns. Reads take no lock.
//
// A gateway built before this lock existed opens no lock file, so it still races
// a new one until every pod runs this release. The lock file is harmless to it.
const (
	// workspaceLockWait bounds how long a push or an open waits for the lock. A
	// lock not taken in this time is refused with ErrWorkspaceLockTimeout, so a
	// stuck peer cannot hold a request open forever.
	workspaceLockWait = 5 * time.Second

	// workspaceLockPoll is how often a waiter retries the lock.
	workspaceLockPoll = 10 * time.Millisecond
)

// osTryLock is the production lock attempt: flockTryLock with the kernel's flock.
func osTryLock(path string) (release func() error, busy bool, err error) {
	return flockTryLock(path, syscall.Flock)
}

// flockTryLock makes one non-blocking attempt at the exclusive flock on path,
// creating the lock file 0600 if it is absent. It returns busy when another
// holder has the lock; release drops it and closes the file. A failed attempt
// closes its file, so nothing is held on a refusal. flock is the kernel call,
// injected so a test can drive a failure the disk does not produce.
func flockTryLock(path string, flock func(fd int, how int) error) (release func() error, busy bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	for {
		err := flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return func() error {
		unlockErr := flock(int(f.Fd()), syscall.LOCK_UN)
		closeErr := f.Close()
		return errors.Join(unlockErr, closeErr)
	}, false, nil
}

// lockPath is the lock file beside the live tree: `<parent>/.<base>.lock`.
func (s *WorkspaceStore) lockPath() string {
	return filepath.Join(s.parent(), "."+s.base()+".lock")
}

// acquireLock takes the workspace lock, polling until the deadline set by the
// store's clock. It returns ErrWorkspaceLockTimeout when the lock is still held
// at the deadline, and ErrWorkspaceLock when the lock file cannot be opened or
// locked at all. A caller that gets no error holds the lock until release runs.
func (s *WorkspaceStore) acquireLock() (func() error, error) {
	deadline := s.fs.now().Add(workspaceLockWait)
	for {
		release, busy, err := s.fs.tryLock(s.lockPath())
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrWorkspaceLock, fsMsg(err))
		}
		if !busy {
			return release, nil
		}
		if !s.fs.now().Before(deadline) {
			return nil, fmt.Errorf("%w: not taken within %s", ErrWorkspaceLockTimeout, workspaceLockWait)
		}
		s.fs.pause(workspaceLockPoll)
	}
}
