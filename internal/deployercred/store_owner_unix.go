//go:build unix

package deployercred

import (
	"io/fs"
	"syscall"
)

// infoOf is Info from the host's lstat. A file info the platform does not report
// as a Unix stat has neither owner nor group: -1 for both, so a missing owner is
// never read as uid 0 and a missing group is never read as gid 0.
func infoOf(fi fs.FileInfo) Info {
	info := Info{Mode: fi.Mode(), Owner: -1, Group: -1}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		info.Owner = int(st.Uid)
		info.Group = int(st.Gid)
	}
	return info
}
