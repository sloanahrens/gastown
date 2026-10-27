package events

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DispatchReader follows the town's events log and answers the dispatch clock,
// reading only the bytes appended since its last call. It is the dashboard's
// reader: the summary poll runs every minute for the panel's whole life, and
// re-parsing a log that only grows would cost more every hour the town runs
// (gt-tzbtf).
//
// A log that was replaced (the prune renames its replacement into place) or
// truncated is read again from the start: the offsets name bytes of a file that
// is gone, so continuing from them would fold in a middle. A truncation that
// regrows past the offset between two polls is indistinguishable from an append
// (the limit Tail documents). Only complete lines are folded in, so a line a
// writer is still appending is read whole on the next call.
type DispatchReader struct {
	path string

	mu     sync.Mutex
	clock  *DispatchClock
	offset int64
	file   os.FileInfo // the file offset counts bytes of; nil before the first read
}

// NewDispatchReader returns a reader of the town's events log.
func NewDispatchReader(townRoot string) *DispatchReader {
	return &DispatchReader{path: filepath.Join(townRoot, EventsFile)}
}

// Starts returns each dispatched bead's clock: when its current run went out
// (DispatchStarts). A missing log is a town that dispatched nothing, so it
// reads as an empty clock rather than an error; a log that exists but cannot be
// read is an error, and the caller shows the reading as unavailable rather than
// as a town where every bead was dispatched a moment ago.
func (r *DispatchReader) Starts() (map[string]time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fi, err := os.Stat(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			// No log: no dispatch to date, and a log that appears later is read
			// from its start.
			r.clock, r.offset, r.file = NewDispatchClock(), 0, nil
			return map[string]time.Time{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", r.path, err)
	}
	if r.clock != nil && r.file != nil && os.SameFile(fi, r.file) && fi.Size() == r.offset {
		return r.clock.Starts(), nil // nothing appended since the last read
	}

	f, err := os.Open(r.path) //nolint:gosec // G304: path is the town's own runtime file
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", r.path, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	clock, offset := r.clock, r.offset
	if clock == nil || r.file == nil || !os.SameFile(fi, r.file) || fi.Size() < offset {
		// First read, a replaced log, or a truncated one: start over rather
		// than fold new bytes into a map built from a file that is gone.
		clock, offset = NewDispatchClock(), 0
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("reading %s: %w", r.path, err)
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", r.path, err)
	}

	// Fold in whole lines only: anything after the last newline is a line a
	// writer has not finished, and the next call reads it from its first byte.
	if end := bytes.LastIndexByte(data, '\n'); end >= 0 {
		clock.Add(decodeEventLines(data[:end+1]))
		offset += int64(end + 1)
	}
	r.clock, r.offset, r.file = clock, offset, fi
	return clock.Starts(), nil
}

// decodeEventLines reads every complete line of b as an event, skipping the
// lines that are not one (decodeEventLine).
func decodeEventLines(b []byte) []Event {
	var out []Event
	for _, line := range bytes.Split(b, []byte("\n")) {
		if event, ok := decodeEventLine(string(line)); ok {
			out = append(out, event)
		}
	}
	return out
}
