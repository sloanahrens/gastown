// Package townbackup owns the layout of the nightly town-state backups
// (gt-832hyx): one dated directory per night under the backup root
// (Root: <home>/gt-backups/town-state), outside the town root.
//
//	<root>/2026-11-18/state.tar.gz   the exported town state
//	<root>/2026-11-18/backup.json    manifest, written last
//
// A night is written into <date>.partial and renamed to <date> once its archive
// is in and backup.json is written, so a directory named for a date is always
// complete. Rotation keeps the newest Keep. Producing the archive and checking
// its contents belong to the caller.
package townbackup

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
	Keep = 14

	// StaleAfter is the age past which the newest backup counts as stale: one
	// missed night plus the window's slack.
	StaleAfter = 36 * time.Hour

	// ArchiveName is the exported town state inside a night's directory.
	ArchiveName = "state.tar.gz"

	// ManifestName is the manifest inside a night's directory.
	ManifestName = "backup.json"

	dateLayout    = "2006-01-02"
	partialSuffix = ".partial"
)

// Root is the backup root for a home directory: <home>/gt-backups/town-state.
func Root(home string) string {
	return filepath.Join(home, "gt-backups", "town-state")
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

// Manifest records what a night's backup holds, and is written last: a night
// with no manifest never finished.
type Manifest struct {
	Taken  time.Time `json:"taken"`
	Files  int       `json:"files"`
	Bytes  int64     `json:"bytes"`
	SHA256 string    `json:"sha256"`
}

// Backup is one complete night on disk.
type Backup struct {
	Path     string
	Manifest Manifest
}

// Commit writes m into the partial directory of the night containing m.Taken
// and renames it to its final name, in that order. It refuses a night whose
// archive is absent and a night that already exists.
func Commit(root string, m Manifest) (string, error) {
	partial := PartialDir(root, m.Taken)
	final := Dir(root, m.Taken)
	if _, err := os.Stat(final); err == nil {
		return "", fmt.Errorf("backup %s already exists", final)
	}
	archive := filepath.Join(partial, ArchiveName)
	if _, err := os.Stat(archive); err != nil {
		return "", fmt.Errorf("backup %s is not complete: %w", partial, err)
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

// Age is how long ago b was taken, at now.
func (b Backup) Age(now time.Time) time.Duration {
	return now.Sub(b.Manifest.Taken)
}

// Stale reports whether b is older than StaleAfter at now.
func (b Backup) Stale(now time.Time) bool {
	return b.Age(now) > StaleAfter
}

// Rotate removes every complete backup but the newest keep, and every partial
// directory (a night that never finished). It touches only names it owns
// (YYYY-MM-DD and YYYY-MM-DD.partial) and returns what it removed. Call it with
// no backup in flight.
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
				// than keeping one night too many.
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
