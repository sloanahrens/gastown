package land

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// PendingRecord is one landing record a landing could not write to its bead
// after its retries, waiting for a later pass (gt-1951uw).
type PendingRecord struct {
	Bead string    `json:"bead"`
	Kind string    `json:"kind"`
	Key  string    `json:"key"` // idempotency key: one entry per Bead, Kind and Key
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// RecordKindNotes and RecordKindComment are the two writes a queued record names.
const (
	RecordKindNotes   = "notes"   // append Text to the bead's notes
	RecordKindComment = "comment" // add Text as a comment on the bead
)

// RecordQueue is a JSON-lines queue of landing records at the caller's Path.
type RecordQueue struct{ Path string }

// queueEntry is one non-empty line: the record it parses as, and its bytes, so
// a rewrite keeps a torn line and any field this version does not know.
type queueEntry struct {
	rec PendingRecord
	raw string
	ok  bool
}

// recordQueueMu serializes every queue's file operations in the process.
var recordQueueMu sync.Mutex

// walk runs fn over the queue's entries under the process mutex; a missing file
// is an empty queue. The read refuses a symlink as the append does.
func (q RecordQueue) walk(fn func([]queueEntry) error) error {
	recordQueueMu.Lock()
	defer recordQueueMu.Unlock()
	fh, err := os.OpenFile(q.Path, os.O_RDONLY|oNoFollow, 0)
	if os.IsNotExist(err) {
		return fn(nil)
	}
	if err != nil {
		return fmt.Errorf("reading landing record queue: %w", err)
	}
	defer func() { _ = fh.Close() }()
	body, err := io.ReadAll(fh)
	if err != nil {
		return fmt.Errorf("reading landing record queue: %w", err)
	}
	var entries []queueEntry
	for _, raw := range strings.Split(string(body), "\n") {
		if raw = strings.TrimSpace(raw); raw != "" {
			var e queueEntry
			e.raw, e.ok = raw, json.Unmarshal([]byte(raw), &e.rec) == nil
			entries = append(entries, e)
		}
	}
	return fn(entries)
}

// Enqueue appends rec unless an entry with the same Bead, Kind and Key is queued.
func (q RecordQueue) Enqueue(rec PendingRecord) error {
	if rec.Kind != RecordKindNotes && rec.Kind != RecordKindComment {
		return fmt.Errorf("queueing landing record: unknown kind %q", rec.Kind)
	}
	if rec.Bead == "" || rec.Key == "" {
		return fmt.Errorf("queueing landing record: a bead and a key are required")
	}
	return q.walk(func(entries []queueEntry) error {
		for _, e := range entries {
			if e.ok && e.rec.sameEntry(rec) {
				return nil
			}
		}
		line, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("encoding landing record: %w", err)
		}
		if err := appendJSONLine(q.Path, line, nil); err != nil {
			return fmt.Errorf("queueing landing record: %w", err)
		}
		return nil
	})
}

// Pending returns the queued records, oldest first; a torn line is skipped and
// reported in the error, never hiding the records around it.
func (q RecordQueue) Pending() ([]PendingRecord, error) {
	var recs []PendingRecord
	err := q.walk(func(entries []queueEntry) error {
		for _, e := range entries {
			if e.ok {
				recs = append(recs, e.rec)
			}
		}
		if n := len(entries) - len(recs); n > 0 {
			return fmt.Errorf("%d unparsable line(s) skipped in %s; the records around them still read", n, q.Path)
		}
		return nil
	})
	return recs, err
}

// Remove rewrites the queue without rec through a temp file and rename; a
// missing queue or entry is a no-op and a torn line is kept as read.
func (q RecordQueue) Remove(rec PendingRecord) error {
	return q.walk(func(entries []queueEntry) error {
		var kept strings.Builder
		removed := false
		for _, e := range entries {
			if e.ok && e.rec.sameEntry(rec) {
				removed = true
				continue
			}
			kept.WriteString(e.raw + "\n")
		}
		if !removed {
			return nil
		}
		if err := atomicfile.WriteFile(q.Path, []byte(kept.String()), 0o600); err != nil {
			return fmt.Errorf("rewriting landing record queue: %w", err)
		}
		return nil
	})
}

// sameEntry reports whether two records name the same queued write.
func (r PendingRecord) sameEntry(b PendingRecord) bool {
	return r.Bead == b.Bead && r.Kind == b.Kind && r.Key == b.Key
}
