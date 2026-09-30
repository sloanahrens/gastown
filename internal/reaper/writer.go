package reaper

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// Writer is the bd verb surface every reaper write goes through
// (gt-fcxe9.12, ADR 0001). The reaper selects candidates with read-only SQL
// and never writes a bd table itself: bd's close and delete keep is_blocked,
// the events journal, close metadata and bd's own Dolt commit correct, which
// a raw UPDATE or DELETE skipped (B5-06, G1-19). *beads.Beads satisfies it.
type Writer interface {
	// CloseWithReason closes ids ("bd close --reason"); bd refuses an issue
	// with an open blocker or child and reports it as a partial close.
	CloseWithReason(reason string, ids ...string) error
	// ForceCloseWithReason closes ids past bd's close fences
	// ("bd close --reason --force").
	ForceCloseWithReason(reason string, ids ...string) error
	// DeleteIssues permanently deletes ids ("bd delete --force").
	DeleteIssues(ids ...string) error
}

// ErrNoWriter refuses a live run that was given no Writer: the reaper has no
// other way to write, and must not fall back to SQL.
var ErrNoWriter = errors.New("reaper: a live run needs a bd writer")

var _ Writer = (*beads.Beads)(nil)

// WriterForDatabase returns a Writer pinned to the beads directory whose
// metadata.json names dbName, so bd writes to exactly the database the
// candidates were read from. Prefix routing is off: an id's prefix does not
// get to send the write to another database.
func WriterForDatabase(townRoot, dbName string) (Writer, error) {
	beadsDir, err := doltserver.BeadsDirForDatabase(townRoot, dbName)
	if err != nil {
		return nil, fmt.Errorf("resolve bd writer for %s: %w", dbName, err)
	}
	return beads.NewRigLocal(filepath.Dir(beadsDir)), nil
}

// closeInChunks closes ids DefaultBatchSize at a time through close and
// returns how many closed. A partial close counts only the ids bd closed; a
// failed chunk does not stop the rest, and every failure is returned.
func closeInChunks(ids []string, close func(ids ...string) error) (int, error) {
	closed := 0
	var errs []error
	for start := 0; start < len(ids); start += DefaultBatchSize {
		chunk := ids[start:min(start+DefaultBatchSize, len(ids))]
		err := close(chunk...)
		closed += len(beads.ClosedIDs(chunk, err))
		if err != nil {
			errs = append(errs, err)
		}
	}
	return closed, errors.Join(errs...)
}

// deleteInChunks deletes ids DefaultBatchSize at a time and returns how many
// were deleted. bd deletes a chunk all or nothing.
func deleteInChunks(w Writer, ids []string) (int, error) {
	deleted := 0
	var errs []error
	for start := 0; start < len(ids); start += DefaultBatchSize {
		chunk := ids[start:min(start+DefaultBatchSize, len(ids))]
		if err := w.DeleteIssues(chunk...); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted += len(chunk)
	}
	return deleted, errors.Join(errs...)
}
