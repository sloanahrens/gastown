//go:build unix

package main

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

// TestUnlockReleasesTheLockWhileACopyOfTheFDIsOpen keeps a duplicate of the
// lock's descriptor across the release, as a forked child carries one until it
// execs. flock belongs to the open file description, so a release that only
// closes the parent's descriptor leaves the lock held for the copy and the
// next run is refused (gt-s8gp1, the same class as gt-ubj9e).
func TestUnlockReleasesTheLockWhileACopyOfTheFDIsOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unlock, err := lockBackupDir(dir)
	if err != nil {
		t.Fatalf("lockBackupDir: %v", err)
	}

	copyFD, err := dupLockDescriptor(dir)
	if err != nil {
		t.Fatalf("find the lock descriptor: %v", err)
	}
	syscall.CloseOnExec(copyFD)
	t.Cleanup(func() { _ = syscall.Close(copyFD) })

	unlock()

	again, err := lockBackupDir(dir)
	if err != nil {
		t.Fatalf("lockBackupDir after the release with a copy of the descriptor open: %v", err)
	}
	again()
}

// dupLockDescriptor duplicates the descriptor this process holds on dir/.lock.
// lockBackupDir keeps its open file to itself, so the descriptor is found by
// matching its device and inode against the lock path's own stat.
func dupLockDescriptor(dir string) (int, error) {
	path := filepath.Join(dir, ".lock")
	var want syscall.Stat_t
	if err := syscall.Stat(path, &want); err != nil {
		return -1, err
	}
	for fd := 3; fd <= 1023; fd++ {
		var got syscall.Stat_t
		if err := syscall.Fstat(fd, &got); err != nil {
			continue
		}
		if got.Dev == want.Dev && got.Ino == want.Ino {
			return syscall.Dup(fd)
		}
	}
	return -1, errors.New("no open descriptor on the lock file")
}
