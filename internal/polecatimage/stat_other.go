//go:build !linux

package polecatimage

import (
	"errors"
	"os"
	"time"
)

// lastUsed is the modification time on platforms without the Linux access-time
// read. The trim helper runs only in the polecat image, whose kernel is Linux,
// so the fallback is not reached in production (gt-3g7p2). Darwin is build-only
// since the gt-wrap cutover (gt-qq5kv4): nothing runs the trim helpers there.
func lastUsed(info os.FileInfo) time.Time {
	return info.ModTime()
}

// filesystemFree is unsupported on platforms without the Linux statfs read, and
// the list writer refuses rather than writing a list it could not size against
// the claim's room. The trim helpers run in the polecat image, whose kernel is
// Linux (gt-3g7p2). Darwin is build-only since the gt-wrap cutover (gt-qq5kv4):
// nothing runs the trim helpers there.
func filesystemFree(string) (int64, error) {
	return 0, errors.New("cannot measure the filesystem's free space on this platform")
}
