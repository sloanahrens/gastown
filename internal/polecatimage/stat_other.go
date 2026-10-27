//go:build !linux && !darwin

package polecatimage

import (
	"os"
	"time"
)

// lastUsed is the modification time where this platform's file info carries no
// access time. The trim helper runs only in the polecat image, whose kernel is
// Linux (gt-3g7p2).
func lastUsed(info os.FileInfo) time.Time {
	return info.ModTime()
}
