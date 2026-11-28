package gocacheprog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// IndexEntry is one action's output, its size and last use. PutTime is the time of
// the put that set OutputID and Size; Merge compares a later put against it.
type IndexEntry struct {
	OutputID string
	Size     int64
	PutTime  time.Time
	LastUse  time.Time
}

// Index maps an action id to its entry.
type Index map[string]IndexEntry

// Merge returns idx with records applied, leaving idx unchanged. All puts apply
// before all uses, so within one call record order and repetition do not matter:
// a use of an action put in the same call is kept, and merging the same records
// again changes nothing.
//
// A use of an action held neither by idx nor by a put of this call is ignored and
// not remembered, so a put in a later call carries its own time as last use. An
// entry never exists without a size, so a caller passes every record it holds in
// one call.
func Merge(idx Index, records []Record) Index {
	out := make(Index, len(idx))
	maps.Copy(out, idx)
	for _, r := range records {
		if r.Op != opPut {
			continue
		}
		e, ok := out[r.ActionID]
		if !ok || putWins(r, e) {
			e.OutputID, e.Size, e.PutTime = r.OutputID, r.Size, r.Time
		}
		if r.Time.After(e.LastUse) {
			e.LastUse = r.Time
		}
		out[r.ActionID] = e
	}
	// A use of an unknown action is dropped: an entry never exists without a size.
	for _, r := range records {
		if e, ok := out[r.ActionID]; ok && r.Op == opUse && r.Time.After(e.LastUse) {
			e.LastUse = r.Time
			out[r.ActionID] = e
		}
	}
	return out
}

// putWins reports whether put r replaces e's output and size: later time, then greater output id, then greater size.
func putWins(r Record, e IndexEntry) bool {
	if !r.Time.Equal(e.PutTime) {
		return r.Time.After(e.PutTime)
	}
	if r.OutputID != e.OutputID {
		return r.OutputID > e.OutputID
	}
	return r.Size > e.Size
}

// snapshotLine is one line of the snapshot file: an action id and its entry.
type snapshotLine struct {
	Action string    `json:"action"`
	Output string    `json:"output"`
	Size   int64     `json:"size"`
	Put    time.Time `json:"put"`
	Use    time.Time `json:"use"`
}

// ReadSnapshot reads the index snapshot at path. A missing file is an empty index.
// Any other problem is an error naming the file: sizes cannot be rebuilt without the
// snapshot, so a damaged one is never read as empty.
func ReadSnapshot(path string) (Index, error) {
	idx := Index{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gocacheprog: snapshot %s: %w", path, err)
	}
	// WriteSnapshot ends every line with a newline, so text after the last one was cut off.
	lines := strings.Split(string(data), "\n")
	if lines[len(lines)-1] != "" {
		return nil, fmt.Errorf("gocacheprog: snapshot %s: last line is cut off", path)
	}
	for n, line := range lines[:len(lines)-1] {
		action, e, err := decodeSnapshotLine([]byte(line))
		if _, dup := idx[action]; err == nil && dup {
			err = errors.New("repeated action")
		}
		if err != nil {
			return nil, fmt.Errorf("gocacheprog: snapshot %s: line %d: %w", path, n+1, err)
		}
		idx[action] = e
	}
	return idx, nil
}

// decodeSnapshotLine decodes one snapshot line and refuses an entry the reader cannot
// trust: ids must be hex, size non-negative, put and last-use times set.
func decodeSnapshotLine(line []byte) (string, IndexEntry, error) {
	var l snapshotLine
	if err := json.Unmarshal(line, &l); err != nil {
		return "", IndexEntry{}, err
	}
	switch {
	case !validID([]byte(l.Action)):
		return "", IndexEntry{}, errors.New("action id is empty or not hex")
	case !validID([]byte(l.Output)):
		return "", IndexEntry{}, errors.New("output id is empty or not hex")
	case l.Size < 0:
		return "", IndexEntry{}, errors.New("negative size")
	case l.Put.IsZero():
		return "", IndexEntry{}, errors.New("no put time")
	case l.Use.IsZero():
		return "", IndexEntry{}, errors.New("no last use")
	}
	return l.Action, IndexEntry{OutputID: l.Output, Size: l.Size, PutTime: l.Put, LastUse: l.Use}, nil
}

// WriteSnapshot writes idx to path as one JSON line per entry, in action id order,
// through atomicfile so a reader sees the old snapshot or the new one.
func WriteSnapshot(path string, idx Index) error {
	var data []byte
	for _, action := range slices.Sorted(maps.Keys(idx)) {
		e := idx[action]
		line, err := json.Marshal(snapshotLine{Action: action, Output: e.OutputID, Size: e.Size, Put: e.PutTime, Use: e.LastUse})
		if err != nil {
			return fmt.Errorf("gocacheprog: snapshot %s: encode %s: %w", path, action, err)
		}
		data = append(append(data, line...), '\n')
	}
	if err := atomicfile.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("gocacheprog: snapshot %s: %w", path, err)
	}
	return nil
}
