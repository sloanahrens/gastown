// Package installpending reads and writes the pending-install marker,
// <daemon dir>/install-pending.json (gt-vc34c).
//
// The rebuilding job writes it when it concludes a binary-affecting install is
// due and clears it when that cycle reaches a verdict, so a reader outside the
// job — the sling path first — can stay out of an install's way. Pending states
// what makes a marker stand; the file is plain JSON so a shell can read it.
package installpending

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// FileName is the marker's name inside the daemon runtime dir.
const FileName = "install-pending.json"

// Ceiling is how long a marker outlives the cycle that last wrote it before
// readers treat it as gone: longer than one install cycle may take, build and
// gate wait included, so work in flight is never read as abandoned, and short
// enough that a writer which stopped refreshing cannot hold the town out of
// slings for long (gt-vc34c).
const Ceiling = 45 * time.Minute

// Marker is the marker's content: when a cycle concluded the install was due,
// and the process that concluded it.
type Marker struct {
	At  time.Time `json:"at"`
	PID int       `json:"pid"`
}

// Dir is the daemon runtime dir for townRoot, honoring the same override the
// install lock and scripts/install-gt.sh do.
func Dir(townRoot string) string { return dirFor(townRoot, os.Getenv) }

func dirFor(townRoot string, getenv func(string) string) string {
	if dir := getenv("INSTALL_GT_DAEMON_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(townRoot, "daemon")
}

// Path is the marker file for townRoot.
func Path(townRoot string) string { return filepath.Join(Dir(townRoot), FileName) }

// Write records an install that is due at at, written by pid, replacing any
// marker already there so that Ceiling measures from the last cycle that still
// found the install due.
func Write(townRoot string, at time.Time, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("install-pending marker needs a writer pid, got %d", pid)
	}
	return atomicfile.EnsureDirAndWriteJSON(Path(townRoot), Marker{At: at.UTC(), PID: pid})
}

// Clear removes the marker; one that is already gone is not an error.
func Clear(townRoot string) error {
	err := os.Remove(Path(townRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Pending returns the marker in force at now, or nil when no install stands:
// no marker, one whose writer is gone, or one older than Ceiling. A marker
// file that cannot be read or parsed is an error rather than nil, so a caller
// that must not race an install never takes a broken marker for none.
func Pending(townRoot string, now time.Time) (*Marker, error) {
	path := Path(townRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if m.At.IsZero() || m.PID <= 0 {
		return nil, fmt.Errorf("parsing %s: the marker names no time or no writer pid", path)
	}
	// Age is read before the pid: a marker no cycle has refreshed is spent
	// whatever its pid now names, and a pid handed to an unrelated process
	// must not revive it.
	if now.Sub(m.At) >= Ceiling {
		return nil, nil
	}
	if !processAlive(m.PID) {
		return nil, nil
	}
	return &m, nil
}
