package land

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

// The ready stamp is how a writer of the ready-to-land label tells the rig's
// landing worker that the board changed.
//
// An idle rig's worker otherwise lists the ready label every pass to find the
// first submission. Instead it remembers when it last listed the board and
// whether that list was empty, and skips the list while it was empty and no
// stamp is newer than it. Each writer - gt done, gt land requeue and the
// red-main owner's revert queue - touches this stamp after its label write. A
// label written by hand with bd writes no stamp, so the worker's three-minute
// ceiling bounds that delay.
//
// The file is read and written only by this package's helpers, so a stamp that
// exists but cannot be read is not evidence that nothing was submitted: the
// worker lists (fail open).
const ReadyStampFile = "ready-to-land-stamp"

// readyStampDirs is the per-rig runtime directory the stamp shares with the
// dock stamp, under the town's .runtime (gitignored, local to the town the
// daemon runs in).
const readyStampDirs = "rigs"

// readyRigName is the shape of a directory name under the town: the characters
// a rig name is made of, and nothing that could leave the directory.
var readyRigName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ReadyStampPath is the stamp file for rigName in townRoot. A town root or rig
// name that could escape its directory is refused rather than joined.
func ReadyStampPath(townRoot, rigName string) (string, error) {
	if townRoot == "" {
		return "", errors.New("empty town root for a ready stamp")
	}
	if !readyRigName.MatchString(rigName) || rigName == "." || rigName == ".." {
		return "", fmt.Errorf("invalid rig name %q for a ready stamp", rigName)
	}
	return filepath.Join(townRoot, constants.DirRuntime, readyStampDirs, rigName, ReadyStampFile), nil
}

// ReadyStamp reports when rigName's ready-to-land list was last told to change,
// or the zero time when no writer has stamped it yet.
//
// A stamp that exists but cannot be stat'd is an error: the caller lists
// rather than read a failed stat as "nothing was submitted".
func ReadyStamp(townRoot, rigName string) (time.Time, error) {
	return readyStamp(townRoot, rigName, osReadyStampFS)
}

// TouchReadyStamp records that rigName's ready-to-land list changed, at now.
func TouchReadyStamp(townRoot, rigName string, now time.Time) error {
	return touchReadyStamp(townRoot, rigName, now, osReadyStampFS)
}

// readyStampFS is the filesystem the stamp is read and written through, so a
// test can take the failure paths a writable host does not offer: the worker
// has to list through a stamp it cannot read, and a writer has to report a
// stamp it could not leave.
type readyStampFS struct {
	stat      func(path string) (fs.FileInfo, error)
	mkdirAll  func(path string, perm os.FileMode) error
	writeFile func(path string, data []byte, perm os.FileMode) error
	chtimes   func(path string, atime, mtime time.Time) error
}

var osReadyStampFS = readyStampFS{
	stat:      os.Stat,
	mkdirAll:  os.MkdirAll,
	writeFile: os.WriteFile,
	chtimes:   os.Chtimes,
}

// readyStamp is ReadyStamp through fsys.
func readyStamp(townRoot, rigName string, fsys readyStampFS) (time.Time, error) {
	path, err := ReadyStampPath(townRoot, rigName)
	if err != nil {
		return time.Time{}, err
	}
	fi, err := fsys.stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the ready stamp %s: %w", path, err)
	}
	return fi.ModTime(), nil
}

// touchReadyStamp is TouchReadyStamp through fsys.
//
// The stamp's mtime is held strictly newer than the file it replaces, because
// a worker compares it with a list it took before this write: two submissions
// inside the filesystem's timestamp resolution - or a clock that steps back
// between them - would otherwise melt into one and leave the second waiting
// for the worker's ceiling. The step is a second rather than a nanosecond so
// it survives a filesystem that stores whole seconds.
func touchReadyStamp(townRoot, rigName string, now time.Time, fsys readyStampFS) error {
	path, err := ReadyStampPath(townRoot, rigName)
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
		return fmt.Errorf("writing the ready stamp for rig %s: %w", rigName, err)
	}
	if err := fsys.chtimes(path, stamp, stamp); err != nil {
		return fmt.Errorf("stamping the ready file for rig %s: %w", rigName, err)
	}
	return nil
}
