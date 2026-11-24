package reconcile

import (
	"errors"
	"io/fs"
	"strings"
	"time"
)

// ArchiveLastGood returns the newest modification time among the dolt-log
// archive files directly in dir, so the archive duty can judge the age of its
// last good run. A dir that does not exist, or holds no such file, is
// ErrNoResult; any other read error is returned unchanged.
func ArchiveLastGood(fsys fs.FS, dir string) (time.Time, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, ErrNoResult
		}
		return time.Time{}, err
	}
	var newest time.Time
	for _, entry := range entries {
		// events-* files are written by another step, so they do not date this
		// duty; subdirectories are never archives.
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "dolt-log-") || !strings.HasSuffix(name, ".csv.gz") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return time.Time{}, err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return time.Time{}, ErrNoResult
	}
	return newest, nil
}
