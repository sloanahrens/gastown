//go:build !unix

package deployercred

import "io/fs"

// infoOf is Info from the host's lstat. This platform reports neither owner nor
// group, so both are -1: an unknown owner is never read as uid 0, nor an unknown
// group as gid 0.
func infoOf(fi fs.FileInfo) Info {
	return Info{Mode: fi.Mode(), Owner: -1, Group: -1}
}
