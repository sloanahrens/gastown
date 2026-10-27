//go:build darwin

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
	if atime := time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec); atime.After(used) {
		return atime
	}
	return used
}
