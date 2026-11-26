package runnerroll

import (
	"context"
	"path/filepath"
	"time"
)

// Backer is an optional Target interface: a target that writes the live objects
// it is about to replace, so the changed objects can be restored by hand.
type Backer interface {
	// Backup writes the live objects under dir and returns their file names.
	Backup(ctx context.Context, dir string) ([]string, error)
}

// backupStampFormat stamps a backup directory in UTC, so a directory sorts by
// the time of its roll whichever zone the host runs in.
const backupStampFormat = "20060102T150405Z"

// BackupDir is the directory a roll at townRoot writes the live objects in at
// now: <townRoot>/.runtime/runner-roll/<UTC stamp>.
func BackupDir(townRoot string, now time.Time) string {
	return filepath.Join(townRoot, ".runtime", "runner-roll", now.UTC().Format(backupStampFormat))
}
