package reconcile

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"time"
)

// backupDateLayout is the daemon's dated backup directory name, <root>/<date>/<database>.
const backupDateLayout = "2006-01-02"

// BackupLastGood returns the modification time of the newest complete backup night under root: the newest directory named YYYY-MM-DD that holds a subdirectory for every name in databases. It returns ErrNoResult when no such night exists or root does not exist.
func BackupLastGood(fsys fs.FS, root string, databases []string) (time.Time, error) {
	if len(databases) == 0 {
		return time.Time{}, errors.New("backup: no databases listed")
	}
	for _, db := range databases {
		if db != path.Base(db) || db == "." || db == ".." {
			return time.Time{}, fmt.Errorf("backup: %q is not a database name", db)
		}
	}
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, ErrNoResult
		}
		return time.Time{}, fmt.Errorf("reading the backup root %s: %w", root, err)
	}
	var newest time.Time
	found := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// A night is committed by renaming <date>.partial to <date>, so the
		// partial name fails this parse and only committed nights count.
		if _, err := time.Parse(backupDateLayout, entry.Name()); err != nil {
			continue
		}
		night := path.Join(root, entry.Name())
		complete, err := nightComplete(fsys, night, databases)
		if err != nil {
			return time.Time{}, err
		}
		if !complete {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return time.Time{}, fmt.Errorf("reading the backup night %s: %w", night, err)
		}
		if !found || info.ModTime().After(newest) {
			newest, found = info.ModTime(), true
		}
	}
	if !found {
		return time.Time{}, ErrNoResult
	}
	return newest, nil
}

// nightComplete reports whether every database names a subdirectory in night.
func nightComplete(fsys fs.FS, night string, databases []string) (bool, error) {
	entries, err := fs.ReadDir(fsys, night)
	if err != nil {
		return false, fmt.Errorf("reading the backup night %s: %w", night, err)
	}
	held := make(map[string]bool, len(entries))
	for _, entry := range entries {
		held[entry.Name()] = entry.IsDir()
	}
	for _, db := range databases {
		if !held[db] {
			return false, nil
		}
	}
	return true, nil
}
