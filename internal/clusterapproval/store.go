package clusterapproval

import (
	"errors"
	"fmt"
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
