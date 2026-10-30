//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockBackupDir takes the directory's single-run lock and returns the function
// that releases it. A second run finds the lock held and fails at once.
func lockBackupDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: the lock is in the -dir the operator chose
	if err != nil {
		return nil, fmt.Errorf("open backup lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another trackerbackup run holds the lock in %s", dir)
		}
		return nil, fmt.Errorf("lock %s: %w", dir, err)
	}
	return func() {
		// flock belongs to the open file description, not to this descriptor:
		// a child another goroutine forked while the lock was held keeps a
		// copy of the description, and the lock, alive until it execs.
		// Closing alone would leave the lock held after this release;
		// LOCK_UN drops it for every holder of the description (gt-ubj9e,
		// the same class as gt-0ph7q).
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
