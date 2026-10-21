//go:build !unix

package logtriage

import "os"

// fileID has no inode to read off this platform. It reports zero, so a
// rotated log is caught only when it shrinks below the cursor's offset.
func fileID(os.FileInfo) uint64 {
	return 0
}
