//go:build !unix

package deployercred

import "io/fs"

// infoOf is Info from the host's lstat. This platform reports no owner, so Store
// refuses the path: an unknown owner is never read as uid 0.
func infoOf(fi fs.FileInfo) Info {
	return Info{Mode: fi.Mode(), Owner: -1}
}
