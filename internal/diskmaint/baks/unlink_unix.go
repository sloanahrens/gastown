//go:build unix

package baks

import "golang.org/x/sys/unix"

// unlinkFile removes path as a file and never as a directory. os.Remove falls
// back to rmdir when unlink fails, so a directory that replaced the file between
// the check and the removal would be deleted; an unlink fails on it instead.
func unlinkFile(path string) error {
	return unix.Unlink(path)
}
