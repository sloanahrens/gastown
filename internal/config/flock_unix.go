//go:build unix

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockConfigFile takes an exclusive advisory lock on path+".lock", blocking
// until it is free. The lock is per file, across processes.
func lockConfigFile(path string) (unlock func(), err error) {
	return lockConfigFileWait(path, syscall.LOCK_EX)
}

// lockConfigFileNoWait takes the same exclusive lock on the same lock file,
// without waiting: a lock another process holds is ErrConfigLocked, so a
// caller that must not queue behind a writer refuses instead (gt-yjzepk.4).
func lockConfigFileNoWait(path string) (func(), error) {
	return lockConfigFileWait(path, syscall.LOCK_EX|syscall.LOCK_NB)
}

func lockConfigFileWait(path string, how int) (func(), error) {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: lock path derives from an internal config path
	if err != nil {
		return nil, fmt.Errorf("opening lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		// EWOULDBLOCK is EAGAIN: the lock is taken, and LOCK_NB asked not to queue.
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrConfigLocked, lockPath)
		}
		return nil, fmt.Errorf("locking %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
