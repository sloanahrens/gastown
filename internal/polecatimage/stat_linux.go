//go:build linux

package polecatimage

import (
	"os"
	"syscall"
	"time"
)

// lastUsed is the later of a file's access and modification times: a noatime
// or relatime mount leaves the access time stale, while Go touches the
// modification time of a cache entry it reuses, so either alone can name the
// wrong file as the oldest (gt-3g7p2).
func lastUsed(info os.FileInfo) time.Time {
	used := info.ModTime()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return used
	}
	// The conversion is a no-op on 64-bit Linux and needed on 32-bit, where
	// Timespec's fields are int32.
	if atime := time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec)); atime.After(used) { //nolint:unconvert
		return atime
	}
	return used
}

// filesystemFree is the room the filesystem holding root can still take, in bytes: what the
// claim-carried list writer reads before it writes a list into a claim that may be over its policy
// cap (gt-2rq36).
func filesystemFree(root string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail * uint64(st.Bsize)), nil
}
