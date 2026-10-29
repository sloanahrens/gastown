//go:build !unix

package baks

import "errors"

// errUnlinkUnsupported reports a platform with no unlink that refuses a
// directory. os.Remove there may fall back to rmdir, so a removal could delete
// a directory that replaced the planned file; refusing keeps it instead.
var errUnlinkUnsupported = errors.New("baks: this platform cannot unlink a file safely")

// unlinkFile reports that this platform has no removal that refuses a directory,
// so nothing is removed.
func unlinkFile(string) error {
	return errUnlinkUnsupported
}
