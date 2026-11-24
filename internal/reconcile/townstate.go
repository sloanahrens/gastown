package reconcile

import (
	"errors"
	"io/fs"
	"path"
	"time"
)

// townStateArchive is the archive file each nightly town-state backup writes.
const townStateArchive = "state.tar.gz"

// TownStateLastGood returns the modification time of the newest state.tar.gz
// under root's per-night directories (<root>/<YYYY-MM-DD>/state.tar.gz); only a
// regular file in a YYYY-MM-DD directory counts. A missing root, or one holding
// no such archive, is ErrNoResult; any other read error is returned.
func TownStateLastGood(fsys fs.FS, root string) (time.Time, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, ErrNoResult
		}
		return time.Time{}, err
	}
	var newest time.Time
	for _, entry := range entries {
		if _, err := time.Parse(backupDateLayout, entry.Name()); err != nil || !entry.IsDir() {
			continue
		}
		info, err := fs.Stat(fsys, path.Join(root, entry.Name(), townStateArchive))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return time.Time{}, err
		}
		if info.Mode().IsRegular() && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return time.Time{}, ErrNoResult
	}
	return newest, nil
}
