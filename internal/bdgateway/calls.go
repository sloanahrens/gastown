package bdgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// maxCallReadBytes bounds what ReadCalls reads: the live log's tail, where the
// dashboard's recent window lives. A full 50 MB file is never loaded into the
// gateway's memory.
const maxCallReadBytes int64 = 8 << 20

// CallEntry is one call the log recorded: exactly the fields Log writes, so a
// flag's value, a title or a comment body cannot appear in an entry the
// dashboard serves.
type CallEntry struct {
	TS     string `json:"ts"`
	Class  string `json:"class"`
	Caller string `json:"caller"`
	// Actor is the acting agent the caller named in BEADS_ACTOR, or BD_ACTOR
	// when that is empty. A call naming neither logs no actor.
	Actor string `json:"actor,omitempty"`
	Verb  string `json:"verb"`
	// Write reports whether the verb is one that may change beads, so a line
	// says whether the call wrote without the reader knowing the policy.
	Write      bool     `json:"write"`
	Flags      []string `json:"flags"`
	Beads      []string `json:"beads"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
	Error      string   `json:"error,omitempty"`
}

// ReadCalls returns the live call log's entries stamped after since, oldest
// first, at most limit; it reads the last maxCallReadBytes of that one file and
// never a rotated one. A missing log is an empty list and no error.
func ReadCalls(dir string, since time.Time, limit int) ([]CallEntry, error) {
	if dir == "" {
		return nil, errors.New("bdgateway: read calls needs a log directory")
	}
	if limit <= 0 {
		return nil, errors.New("bdgateway: read calls needs a positive limit")
	}
	f, err := os.Open(filepath.Join(dir, DefaultCallLogName))
	if errors.Is(err, os.ErrNotExist) {
		return []CallEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bdgateway: open call log: %w", err)
	}
	data, readErr := readCallLogTail(f)
	if closeErr := f.Close(); readErr == nil && closeErr != nil {
		readErr = fmt.Errorf("bdgateway: close call log: %w", closeErr)
	}
	if readErr != nil {
		return nil, readErr
	}

	type stamped struct {
		entry CallEntry
		ts    time.Time
	}
	var kept []stamped
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e CallEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// A torn line is one crash wide; the lines around it still read, as
			// the landings file's reader does (gt-jr90x).
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil {
			// Without a stamp the entry cannot be shown to be after since.
			continue
		}
		if !ts.After(since) {
			continue
		}
		kept = append(kept, stamped{entry: e, ts: ts})
	}
	// Log stamps a call before it queues the line, so a writer preempted in
	// between can leave the file out of stamp order.
	slices.SortStableFunc(kept, func(a, b stamped) int { return a.ts.Compare(b.ts) })
	if len(kept) > limit {
		kept = kept[:limit]
	}
	out := make([]CallEntry, len(kept))
	for i, s := range kept {
		out[i] = s.entry
	}
	return out, nil
}

// callLogFile is the part of *os.File readCallLogTail uses; a test substitutes
// one whose calls fail to drive each error path.
type callLogFile interface {
	Stat() (os.FileInfo, error)
	Seek(offset int64, whence int) (int64, error)
	Read(p []byte) (int, error)
}

// readCallLogTail reads at most the last maxCallReadBytes of f. When the read
// starts inside a line, the fragment up to its first newline is dropped: a
// partial line is not an entry.
func readCallLogTail(f callLogFile) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("bdgateway: measure call log: %w", err)
	}
	offset := max(info.Size()-maxCallReadBytes, 0)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("bdgateway: seek call log: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCallReadBytes))
	if err != nil {
		return nil, fmt.Errorf("bdgateway: read call log: %w", err)
	}
	if offset > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	return data, nil
}
