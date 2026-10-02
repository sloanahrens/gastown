//go:build integration && darwin

package done

import (
	"errors"

	"golang.org/x/sys/unix"
)

// cloneTree clones the directory tree src to dst, which must not exist, with
// one clonefile(2) call. It reports false, with no error, when the
// filesystem cannot clone (not APFS, or src and dst on different volumes),
// so the caller can fall back to copying.
func cloneTree(src, dst string) (bool, error) {
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EXDEV):
		return false, nil
	}
	return false, err
}
