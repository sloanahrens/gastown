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

// OpenRecord opens the approval record <dir>/<id>.json through a directory
// handle it opens and a file it opens inside that handle, so the caller reads
// the bytes the descriptor returns rather than the ones a path named a moment
// earlier. The caller closes the returned file. It checks only that id names one
// file; the owner, group, mode and symlink refusals on the opened handles are
// gt-0grfdc's, so until that lands it trusts the directory it opened. A refusal
// names the path and never a byte of a record. A missing directory or record is
// ErrNoApproval; any other open failure is not.
func OpenRecord(fsys deployercred.ReadFS, dir, id string) (deployercred.ReadFile, error) {
	if !isRecordID(id) {
		return nil, fmt.Errorf("the approval id %q is not twelve lower-case hex characters", id)
	}
	target := filepath.Join(dir, id+".json")
	root, err := fsys.OpenDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("the approval directory %s is not there: %w", dir, ErrNoApproval)
	case err != nil:
		return nil, fmt.Errorf("could not open the approval directory %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }() // read-only: a close error cannot change the record
	f, err := root.Open(id + ".json")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("the approval record %s is not there: %w", target, ErrNoApproval)
	case err != nil:
		return nil, fmt.Errorf("could not open the approval record %s: %w", target, err)
	}
	return f, nil
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
// and groupGID are the ids bead gt-0grfdc compares the opened handles against;
// this reader does not use them yet. A refusal names the path and the check and
// never a byte of the record: a record that does not parse is reported without
// ParseRecord's error, which can carry a key or a value from it.
func ReadRecord(fsys deployercred.ReadFS, ownerUID, groupGID int, dir, id string) (Record, error) {
	f, err := OpenRecord(fsys, dir, id)
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
