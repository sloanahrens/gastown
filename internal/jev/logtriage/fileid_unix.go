//go:build unix

package logtriage

import (
	"os"
	"syscall"
)

// fileID is the file's inode number. A rotated log is a new file with a new
// inode, which is how a pass knows to start its cursor over.
func fileID(fi os.FileInfo) uint64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return st.Ino
}
