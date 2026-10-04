//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

func fileInode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// reexecSelf replaces this process with a fresh run of the same binary and
// arguments. It returns only on failure.
func reexecSelf(path string) error {
	return syscall.Exec(path, os.Args, os.Environ())
}
