package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// rigDockedFileTTL is how long one tick's docked verdict answers for a later
// tick: five minutes, the same bound the daemon's identity-bead memo keeps
// (gt-z0dcj). The dock stamp `gt rig dock` and `gt rig undock` write drops a
// record sooner, so the window bounds only a label edited on the identity bead
// by hand, and such an edit is seen within five minutes.
const rigDockedFileTTL = 5 * time.Minute

// rigDockedFileDir is the per-rig verdict directory under the town's runtime
// directory (gitignored, and local to the town the dispatcher runs in).
const rigDockedFileDir = "rig_docked"

// rigDockedRecord is one rig's docked verdict as a tick leaves it for the next:
// the verdict, the dock stamp observed before the read that produced it, and
// when that read ran.
type rigDockedRecord struct {
	Rig     string    `json:"rig"`
	Blocked bool      `json:"blocked"`
	Reason  string    `json:"reason"`
	Stamp   time.Time `json:"dock_stamp"`
	ReadAt  time.Time `json:"read_at"`
}

// fresh reports whether rec may answer a check of rigName made at now, given
// the dock stamp observed for that check. A record from before a gt dock or
// undock (a newer stamp) and one older than the ttl must be read from bd again.
// A read time that has not arrived yet (the clock moved back) does not answer
// either: reading bd is always safe, and a record must not outlive its window.
// A record with no read time at all is already older than the window.
func (rec rigDockedRecord) fresh(rigName string, stamp, now time.Time) bool {
	if rec.Rig != rigName {
		return false
	}
	if stamp.After(rec.Stamp) {
		return false
	}
	if now.Before(rec.ReadAt) {
		return false
	}
	return now.Sub(rec.ReadAt) < rigDockedFileTTL
}

// rigDockedFileName is the shape of a verdict file's name: the characters a rig
// name is made of, and nothing that could leave the directory.
var rigDockedFileName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// rigDockedFilePath is rigName's verdict file in townRoot. A rig name that
// could leave the directory is refused rather than joined.
func rigDockedFilePath(townRoot, rigName string) (string, error) {
	if !rigDockedFileName.MatchString(rigName) || rigName == "." || rigName == ".." {
		return "", fmt.Errorf("invalid rig name %q for a docked verdict file", rigName)
	}
	return filepath.Join(constants.TownRuntimePath(townRoot), "spec_dispatch", rigDockedFileDir, rigName+".json"), nil
}

// rigDockedFiles is the filesystem the verdict files are read and written
// through, so a test can take the corrupt, unreadable and unwritable paths a
// healthy host does not offer. It is a value the memo holds, never a package
// variable a test swaps.
type rigDockedFiles struct {
	readFile  func(path string) ([]byte, error)
	mkdirAll  func(path string, perm os.FileMode) error
	writeFile func(path string, data []byte, perm os.FileMode) error
}

// osRigDockedFiles returns the filesystem the dispatcher uses on a real host.
// writeFile is atomic (temp file and rename), so a reader never parses a torn
// record and a crash between ticks leaves the previous record or the new one.
func osRigDockedFiles() rigDockedFiles {
	return rigDockedFiles{readFile: os.ReadFile, mkdirAll: os.MkdirAll, writeFile: atomicfile.WriteFile}
}

// load returns the record rigName's file holds. It reports false when there is
// no record this check may serve: the file is missing, unreadable, unparseable,
// or names another rig. Every one of those means the caller reads bd as before,
// so a damaged file costs a read, never a verdict.
func (f rigDockedFiles) load(townRoot, rigName string) (rigDockedRecord, bool) {
	path, err := rigDockedFilePath(townRoot, rigName)
	if err != nil {
		return rigDockedRecord{}, false
	}
	raw, err := f.readFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "warning: docked verdict file %s unreadable: %v\n", path, err)
		}
		return rigDockedRecord{}, false
	}
	var rec rigDockedRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: docked verdict file %s corrupt: %v\n", path, err)
		return rigDockedRecord{}, false
	}
	return rec, true
}

// save writes rec to rigName's file. The caller logs the error: a record that
// cannot be written only costs the next tick one bd read. The write is atomic
// (the os filesystem's writeFile is atomicfile.WriteFile, a temp file and a
// rename), so a reader never sees a partially written record.
func (f rigDockedFiles) save(townRoot, rigName string, rec rigDockedRecord) error {
	path, err := rigDockedFilePath(townRoot, rigName)
	if err != nil {
		return err
	}
	if err := f.mkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create docked verdict directory: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode docked verdict: %w", err)
	}
	if err := f.writeFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write docked verdict file: %w", err)
	}
	return nil
}
