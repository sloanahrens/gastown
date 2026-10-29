//go:build !linux && !darwin

package polecatimage

import (
	"errors"
	"os"
	"time"
)

// lastUsed is the modification time where this platform's file info carries no
// access time. The trim helper runs only in the polecat image, whose kernel is
// Linux (gt-3g7p2).
func lastUsed(info os.FileInfo) time.Time {
	return info.ModTime()
}

// filesystemFree is unsupported where the standard library carries no statfs,
// and the list writer refuses rather than writing a list it could not size
// against the claim's room. The trim helpers run in the polecat image, whose
// kernel is Linux (gt-3g7p2).
func filesystemFree(string) (int64, error) {
	return 0, errors.New("cannot measure the filesystem's free space on this platform")
}
