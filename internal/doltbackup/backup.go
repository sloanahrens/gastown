// Package doltbackup owns the layout of the nightly Dolt backups
// (gt-8z769.5): one dated directory per night under ~/gt-backups/dolt, outside
// the town root, holding one Dolt backup per database:
//
//	~/gt-backups/dolt/2026-10-01/backup.json   manifest, written last
//	~/gt-backups/dolt/2026-10-01/hq/           CALL dolt_backup('sync-url', ...)
//	~/gt-backups/dolt/2026-10-01/gt/
//
// The daemon's scheduled_maintenance window writes a night into
// <date>.partial and renames it to <date> once every database is in, so a
// directory named for a date is always complete. Rotation keeps the newest
// Keep. Restoring is docs/dolt-restore.md.
package doltbackup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

const (
	// Keep is how many nightly backups rotation leaves.
	Keep = 7

	// StaleAfter is the age past which the newest backup counts as stale:
	// one missed night plus the window's slack.
	StaleAfter = 36 * time.Hour

	// ManifestName is the manifest inside a night's directory.
	ManifestName = "backup.json"

	dateLayout    = "2006-01-02"
	partialSuffix = ".partial"
)

// Root is the backup root for a home directory: <home>/gt-backups/dolt.
func Root(home string) string {
	return filepath.Join(home, "gt-backups", "dolt")
}

// DefaultRoot is Root for the current user's home directory.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return Root(home), nil
}

// Dir is the final directory of the night that contains t (in t's location).
func Dir(root string, t time.Time) string {
	return filepath.Join(root, t.Format(dateLayout))
}

// PartialDir is where the night that contains t is written before it is
// complete.
func PartialDir(root string, t time.Time) string {
	return Dir(root, t) + partialSuffix
}

// Manifest records what a night's backup holds.
type Manifest struct {
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Databases []string  `json:"databases"`
	// Method names how each database was copied, for the restore procedure.
	Method string `json:"method"`
}

// Backup is one complete night on disk.
type Backup struct {
	Path     string
	Manifest Manifest
}

// Commit writes m into the partial directory of the night containing
// m.Started and renames it to its final name. An existing final directory for
// that night is an error: a night is written once.
func Commit(root string, m Manifest) (string, error) {
	partial := PartialDir(root, m.Started)
	final := Dir(root, m.Started)
	if _, err := os.Stat(final); err == nil {
		return "", fmt.Errorf("backup %s already exists", final)
	}
	if err := atomicfile.WriteJSON(filepath.Join(partial, ManifestName), m); err != nil {
		return "", err
	}
	if err := os.Rename(partial, final); err != nil {
		return "", err
	}
	return final, nil
}

// Taken reports whether the night containing t already has a complete backup.
func Taken(root string, t time.Time) bool {
	_, err := readManifest(Dir(root, t))
	return err == nil
}

// List returns the complete backups under root, oldest first. A missing root
// has none. A dated directory without a readable manifest is not a backup.
func List(root string) ([]Backup, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Backup
	for _, e := range entries {
		if !e.IsDir() || !isDate(e.Name()) {
			continue
		}
		path := filepath.Join(root, e.Name())
		m, err := readManifest(path)
		if err != nil {
			continue
		}
		out = append(out, Backup{Path: path, Manifest: m})
	}
	sort.Slice(out, func(i, j int) bool { return filepath.Base(out[i].Path) < filepath.Base(out[j].Path) })
	return out, nil
}

// Newest returns the newest complete backup under root, if any.
func Newest(root string) (Backup, bool, error) {
	all, err := List(root)
	if err != nil || len(all) == 0 {
		return Backup{}, false, err
	}
	return all[len(all)-1], true, nil
}

// LastFor returns the newest complete backup under root that holds db.
func LastFor(root, db string) (Backup, bool, error) {
	all, err := List(root)
	if err != nil {
		return Backup{}, false, err
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Has(db) {
			return all[i], true, nil
		}
	}
	return Backup{}, false, nil
}

// Age is how long ago b finished, at now.
func (b Backup) Age(now time.Time) time.Duration {
	return now.Sub(b.Manifest.Finished)
}

// Has reports whether b holds database db.
func (b Backup) Has(db string) bool {
	for _, name := range b.Manifest.Databases {
		if name == db {
			return true
		}
	}
	return false
}

// Rotate removes every complete backup but the newest keep, and every
// partial directory (a night that never finished). It touches only names it
// owns (YYYY-MM-DD and YYYY-MM-DD.partial) and returns what it removed. Call
// it with no backup in flight.
func Rotate(root string, keep int) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var complete, doomed []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		switch {
		case isDate(strings.TrimSuffix(name, partialSuffix)) && strings.HasSuffix(name, partialSuffix):
			doomed = append(doomed, name)
		case isDate(name):
			_, err := readManifest(filepath.Join(root, name))
			switch {
			case err == nil:
				complete = append(complete, name)
			case errors.Is(err, fs.ErrNotExist):
				// A dated directory with no manifest never finished.
				doomed = append(doomed, name)
			default:
				// The manifest is there but unreadable — a transient read
				// error, or content we cannot parse. Keep the night: deleting
				// a complete backup over an error we cannot explain is worse
				// than keeping one night too many (G8).
				fmt.Fprintf(os.Stderr, "Warning: keeping backup %s, its manifest could not be read: %v\n", name, err)
			}
		}
	}
	sort.Strings(complete)
	if keep < 0 {
		keep = 0
	}
	if len(complete) > keep {
		doomed = append(doomed, complete[:len(complete)-keep]...)
	}
	sort.Strings(doomed)
	var removed []string
	var errs []error
	for _, name := range doomed {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, name)
	}
	return removed, errors.Join(errs...)
}

func isDate(name string) bool {
	_, err := time.Parse(dateLayout, name)
	return err == nil
}

func readManifest(dir string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse %s: %w", filepath.Join(dir, ManifestName), err)
	}
	return m, nil
}
