//go:build unix

package baks

import "syscall"

// unlinkFile removes path as a file and never as a directory. os.Remove falls
// back to rmdir when unlink fails, so a directory that replaced the file between
// the check and the removal would be deleted; an unlink fails on it instead.
func unlinkFile(path string) error {
	return syscall.Unlink(path)
}
