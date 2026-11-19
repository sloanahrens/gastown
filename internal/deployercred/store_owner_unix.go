//go:build unix

package deployercred

import (
	"io/fs"
	"syscall"
)

// infoOf is Info from the host's lstat. A file info the platform does not report
// as a Unix stat has no owner, which Store refuses; it is never read as uid 0.
func infoOf(fi fs.FileInfo) Info {
	info := Info{Mode: fi.Mode(), Owner: -1}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		info.Owner = int(st.Uid)
	}
	return info
}
