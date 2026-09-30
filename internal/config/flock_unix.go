//go:build unix

package config

import (
	"fmt"
	"os"
	"syscall"
)

// lockConfigFile takes an exclusive advisory lock on path+".lock", blocking
// until it is free. The lock is per file, across processes.
func lockConfigFile(path string) (unlock func(), err error) {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: lock path derives from an internal config path
	if err != nil {
		return nil, fmt.Errorf("opening lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
