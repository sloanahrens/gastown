package clusterapproval

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/deployercred"
)

// ErrNoApproval reports that there is no approval record, which the runner
// reads as "not approved yet" rather than as a refused approval.
var ErrNoApproval = errors.New("clusterapproval: there is no approval record")

// recordDirMode and recordFileMode are the exact modes the reader accepts. An
// exact 0640 also refuses a symlink or device, which carry type bits.
const (
	recordDirMode  = fs.ModeDir | 0o750
	recordFileMode = fs.FileMode(0o640)
)

// OpenRecord opens the approval record <dir>/<id>.json through a directory
// handle and a file opened inside it, so the caller reads the bytes the
// descriptor returns. Before any read it refuses, naming the path and the check
// and never a byte of the record: a symlink on dir or a parent; an opened
// directory or record whose mode is not exactly recordDirMode or recordFileMode,
// or whose owner or group is not ownerUID or groupGID. The checks are on the
// handles read, so a swap after the Lstat cannot change the bytes. A missing
// directory or record is ErrNoApproval; any other open failure is not.
func OpenRecord(fsys deployercred.ReadFS, ownerUID, groupGID int, dir, id string) (deployercred.ReadFile, error) {
	if !isRecordID(id) {
		return nil, fmt.Errorf("the approval id %q is not twelve lower-case hex characters", id)
	}
	target := filepath.Join(dir, id+".json")
	if err := refuseSymlinkChain(fsys, dir); err != nil {
		return nil, err
	}
	root, err := fsys.OpenDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("the approval directory %s is not there: %w", dir, ErrNoApproval)
	case err != nil:
		return nil, fmt.Errorf("could not open the approval directory %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }() // read-only: a close error cannot change the record
	dirInfo, err := root.Stat()
	if err != nil {
		return nil, fmt.Errorf("could not read the approval directory %s: %w", dir, err)
	}
	if err := checkOpened("the approval directory "+dir, dirInfo, recordDirMode, ownerUID, groupGID); err != nil {
		return nil, err
	}
	f, err := root.Open(id + ".json")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("the approval record %s is not there: %w", target, ErrNoApproval)
	case err != nil:
		return nil, fmt.Errorf("could not open the approval record %s: %w", target, err)
	}
	fileInfo, err := f.Stat()
	if err == nil {
		err = checkOpened("the approval record "+target, fileInfo, recordFileMode, ownerUID, groupGID)
	}
	if err != nil {
		_ = f.Close() // refused before any read: a close error changes nothing
		return nil, err
	}
	return f, nil
}

// refuseSymlinkChain refuses a symlink at dir or at any parent above it: a
// symlink anywhere on the way would move the read to a place nobody verified. A
// missing component is not refused here, since OpenDir reports a missing
// directory as ErrNoApproval. A component that cannot be read is an error, never
// read as absent.
func refuseSymlinkChain(fsys deployercred.ReadFS, dir string) error {
	for p := dir; ; p = filepath.Dir(p) {
		info, err := fsys.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Keep climbing: the parents of a missing component still need the check.
		case err != nil:
			return fmt.Errorf("could not read %s on the way to the approval directory %s: %w", p, dir, err)
		case info.Mode&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symlink, and the approval record is read only through real directories", p)
		}
		if parent := filepath.Dir(p); parent == p {
			return nil
		}
	}
}

// checkOpened refuses an opened handle whose mode is not want exactly, or whose
// owner or group is not ownerUID or groupGID. An owner or group the platform does
// not report (-1) is refused, never matched as uid 0 or gid 0.
func checkOpened(what string, info deployercred.Info, want fs.FileMode, ownerUID, groupGID int) error {
	switch {
	case info.Mode != want:
		return fmt.Errorf("%s has mode %v, not %v", what, info.Mode, want)
	case info.Owner < 0:
		return fmt.Errorf("the owner of %s is not reported on this platform", what)
	case info.Owner != ownerUID:
		return fmt.Errorf("%s is owned by uid %d, not %d", what, info.Owner, ownerUID)
	case info.Group < 0:
		return fmt.Errorf("the group of %s is not reported on this platform", what)
	case info.Group != groupGID:
		return fmt.Errorf("%s is in group %d, not %d", what, info.Group, groupGID)
	}
	return nil
}

// isRecordID reports whether id is twelve lower-case hex characters, the
// proposal id form, so it names exactly one file in the directory and adds no
// path of its own.
func isRecordID(id string) bool {
	if len(id) != proposalIDChars {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// maxRecordBytes bounds the record ReadRecord reads: the broker's record is a
// few hundred bytes, and a file far larger than one is refused rather than read
// whole.
const maxRecordBytes = 4096

// ReadRecord reads and parses the approval record OpenRecord opens. ownerUID
// and groupGID are the owner and group the opened directory and record must
// both hold. A refusal names the path and the check and never a byte of the
// record: a record that does not parse is reported without ParseRecord's error,
// which can carry a key or a value from it.
func ReadRecord(fsys deployercred.ReadFS, ownerUID, groupGID int, dir, id string) (Record, error) {
	f, err := OpenRecord(fsys, ownerUID, groupGID, dir, id)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error cannot change the record
	target := filepath.Join(dir, id+".json")
	// Read one byte past the bound, so a record that fills it is refused rather
	// than parsed from a truncated prefix.
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return Record{}, fmt.Errorf("could not read the approval record %s: %w", target, err)
	}
	if len(data) > maxRecordBytes {
		return Record{}, fmt.Errorf("the approval record %s is larger than %d bytes", target, maxRecordBytes)
	}
	record, err := ParseRecord(data)
	if err != nil {
		return Record{}, fmt.Errorf("approval record %s: not a valid approval record", target)
	}
	return record, nil
}
