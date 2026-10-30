package rig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// The dock stamp is how `gt rig dock` and `gt rig undock` tell a running daemon
// that a rig's identity bead changed.
//
// The daemon serves a rig's identity-bead read from a memo for a window longer
// than its heartbeat, so without a notice a dock would go unseen for that whole
// window. The stamp is a file both sides reach: the command touches it after the
// label write, the daemon compares its mtime with the one its entry recorded,
// and a newer stamp drops the entry. A dock through gt therefore costs one extra
// bead read, and the window bounds only a label edited on the bead by hand.
//
// The mtime is held strictly newer on every rewrite (touchDockStamp says why).
// Only these commands write the file, so a stamp that cannot be read is an error
// the daemon must not read as "no dock happened" (gt-z0dcj).
//
// Before changing how the daemon drops a memoized read, read
// rigOperationalCache.get in internal/daemon/rig_status.go.

// DockStampFile is the file gt rig dock and gt rig undock stamp.
const DockStampFile = "dock-stamp"

// dockStampDirs is the per-rig runtime directory the stamp lives in, under the
// town's .runtime (gitignored, and local to the town the daemon runs in).
const dockStampDirs = "rigs"

// plainRigName is the shape of a directory name under the town: the characters
// a rig name is made of, and nothing that could leave the directory.
var plainRigName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// DockStampPath is the stamp file for rigName in townRoot. A rig name that
// could leave its directory is refused rather than joined.
func DockStampPath(townRoot, rigName string) (string, error) {
	if !plainRigName.MatchString(rigName) || rigName == "." || rigName == ".." {
		return "", fmt.Errorf("invalid rig name %q for a dock stamp", rigName)
	}
	return filepath.Join(townRoot, constants.DirRuntime, dockStampDirs, rigName, DockStampFile), nil
}

// DockStamp reports when rigName's dock state was last stamped by gt, or the
// zero time when it never has.
//
// A stamp that exists but cannot be stat'd is an error: the caller must not read
// a failed stat as "no dock happened".
func DockStamp(townRoot, rigName string) (time.Time, error) {
	return dockStamp(townRoot, rigName, osDockStampFS)
}

// TouchDockStamp records that rigName was just docked or undocked, at now.
func TouchDockStamp(townRoot, rigName string, now time.Time) error {
	return touchDockStamp(townRoot, rigName, now, osDockStampFS)
}

// dockStampFS is the filesystem the stamp is read and written through, so a test
// can take the failure paths a writable host does not offer: the daemon has to
// survive a stamp it cannot read, and the command has to report a dock it could
// not announce.
type dockStampFS struct {
	stat      func(path string) (fs.FileInfo, error)
	mkdirAll  func(path string, perm os.FileMode) error
	writeFile func(path string, data []byte, perm os.FileMode) error
	chtimes   func(path string, atime, mtime time.Time) error
}

var osDockStampFS = dockStampFS{
	stat:      os.Stat,
	mkdirAll:  os.MkdirAll,
	writeFile: os.WriteFile,
	chtimes:   os.Chtimes,
}

// dockStamp is DockStamp through fsys.
func dockStamp(townRoot, rigName string, fsys dockStampFS) (time.Time, error) {
	path, err := DockStampPath(townRoot, rigName)
	if err != nil {
		return time.Time{}, err
	}
	fi, err := fsys.stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the dock stamp %s: %w", path, err)
	}
	return fi.ModTime(), nil
}

// touchDockStamp is TouchDockStamp through fsys.
//
// The stamp's mtime is held strictly newer than the file it replaces, because
// the daemon drops a memo only for a stamp newer than the one it recorded: two
// docks inside the filesystem's timestamp resolution - or a clock that steps
// back between them - would otherwise melt into one, and the second would be
// invisible to a daemon that had read the bead between them. The step is a
// second rather than a nanosecond so it survives a filesystem that stores whole
// seconds.
func touchDockStamp(townRoot, rigName string, now time.Time, fsys dockStampFS) error {
	path, err := DockStampPath(townRoot, rigName)
	if err != nil {
		return err
	}
	if err := fsys.mkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the runtime directory for rig %s: %w", rigName, err)
	}
	stamp := now
	if fi, err := fsys.stat(path); err == nil && !stamp.After(fi.ModTime()) {
		stamp = fi.ModTime().Add(time.Second)
	}
	if err := fsys.writeFile(path, []byte(stamp.UTC().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing the dock stamp for rig %s: %w", rigName, err)
	}
	if err := fsys.chtimes(path, stamp, stamp); err != nil {
		return fmt.Errorf("stamping the dock file for rig %s: %w", rigName, err)
	}
	return nil
}
