package reaper

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

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

const (
	// deleteChunkAttempts is how many times one delete chunk is tried before
	// the reaper gives up on it: the first try and two retries (gt-ujklm).
	deleteChunkAttempts = 3
	// deleteChunkRetryDelay is the wait before the first retry. Each further
	// retry doubles it, so a chunk that fails twice waits 1 s and then 2 s.
	deleteChunkRetryDelay = time.Second
)

// transientDeleteErrors are the substrings of a bd delete failure a retry can
// clear. Each is a marker the repo has seen in real bd or Dolt output, and each
// names a failure where the delete did not apply: a rolled-back conflict, or a
// connection that died before the command ran. A lost reply is left out: the
// delete may have landed, and a retry would then report a landed delete as
// failed (the same line bdSerializationFailure draws in cmd/bd_contention_retry.go,
// gt-ye21). A refusal or a bad id matches none of them and fails at once (gt-ujklm).
var transientDeleteErrors = []string{
	// Serialization failure: Dolt rolled the transaction back, so a retry
	// cannot apply a delete twice (cmd/bd_contention_retry.go, landworker/retry.go).
	"error 1213",
	"serialization failure",
	"try restarting transaction",
	// Lock wait timeout, which the Dolt server layer already treats as retryable (doltserver.go).
	"lock wait timeout",
	// The connection died before bd ran the command (beads/bd_container_retry.go).
	"invalid connection",
}

// isTransientDeleteError reports whether a failed bd delete chunk may succeed
// on a retry. Everything else is permanent or ambiguous: a refusal ("is still
// referenced"), a bad id, a malformed request, or a lost reply fails the chunk
// without waiting (gt-ujklm, gt-n0mcm).
func isTransientDeleteError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sub := range transientDeleteErrors {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

// deleteInChunks deletes ids DefaultBatchSize at a time and returns how many
// were deleted, the chunks that failed, and every failure joined. bd deletes a
// chunk all or nothing, so a failed chunk adds no count and the rest still run.
// A chunk that fails on a transient error is tried deleteChunkAttempts times in
// all (the first try and two retries) with a doubling backoff before it is
// reported failed (gt-ujklm).
func deleteInChunks(w Writer, ids []string) (int, int, []PurgeChunkFailure, error) {
	return deleteInChunksSleeping(w, ids, time.Sleep)
}

// deleteInChunksSleeping is deleteInChunks with the wait between attempts
// injected, so a unit test drives the backoff on a fake and never sleeps.
// retried counts the chunks that needed more than one attempt and then
// succeeded; a chunk that succeeds on a retry still counts as deleted once.
func deleteInChunksSleeping(w Writer, ids []string, sleep func(time.Duration)) (deleted, retried int, failed []PurgeChunkFailure, err error) {
	var errs []error
	for start := 0; start < len(ids); start += DefaultBatchSize {
		chunk := ids[start:min(start+DefaultBatchSize, len(ids))]
		attempts := 0
		var derr error
		for attempts < deleteChunkAttempts {
			attempts++
			if derr = w.DeleteIssues(chunk...); derr == nil {
				break
			}
			if !isTransientDeleteError(derr) {
				break
			}
			if attempts < deleteChunkAttempts {
				sleep(deleteChunkRetryDelay << (attempts - 1))
			}
		}
		if derr != nil {
			failed = append(failed, PurgeChunkFailure{
				FirstID: chunk[0],
				LastID:  chunk[len(chunk)-1],
				Error:   derr.Error(),
			})
			errs = append(errs, derr)
			continue
		}
		deleted += len(chunk)
		if attempts > 1 {
			retried++
		}
	}
	return deleted, retried, failed, errors.Join(errs...)
}
