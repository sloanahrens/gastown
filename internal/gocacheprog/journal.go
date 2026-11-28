package gocacheprog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// Record kinds: the start line opens a journal, put and use follow it.
const (
	opStart = "start"
	opPut   = "put"
	opUse   = "use"
)

// Record is one journal line: the start line, a put of an entry's body or a use of an entry.
type Record struct {
	Op       string    `json:"op"`
	ActionID string    `json:"action,omitempty"`
	OutputID string    `json:"output,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Time     time.Time `json:"time"`
}

// JournalContents is what ReadJournal reads from one journal file.
type JournalContents struct {
	Start       time.Time
	Live        bool // a process held the journal's lock when ReadJournal probed it
	Records     []Record
	Undecodable int // lines after the start line that are not a well-formed put or use
}

// ReadJournal reads the journal at path. The first line must be a start line.
// Before it reads, it probes and releases the journal's lock; Live reports a held lock.
// A journal locked when it appeared and found unlocked here gets no further writes.
// A probe failure other than a held lock is an error naming the path and the lock probe.
// A last line with no newline is skipped, since its writer may still be writing
// it; other lines that are not a well-formed put or use are counted in Undecodable.
func ReadJournal(path string) (JournalContents, error) {
	var out JournalContents
	unlock, err := lock.FlockTryAcquireExisting(path)
	switch {
	case errors.Is(err, lock.ErrFlockHeld):
		out.Live = true
	case err != nil:
		return out, fmt.Errorf("gocacheprog: journal %s: lock probe: %w", path, err)
	default:
		unlock()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("gocacheprog: journal %s: %w", path, err)
	}
	// The text after the last newline is empty or a line still being written.
	lines := strings.Split(string(data), "\n")
	lines = lines[:len(lines)-1]
	var start Record
	if len(lines) == 0 || json.Unmarshal([]byte(lines[0]), &start) != nil ||
		start.Op != opStart || start.Time.IsZero() {
		return out, fmt.Errorf("gocacheprog: journal %s: no usable start line", path)
	}
	out.Start = start.Time
	for _, line := range lines[1:] {
		var r Record
		if json.Unmarshal([]byte(line), &r) != nil || !validRecord(r) {
			out.Undecodable++
			continue
		}
		out.Records = append(out.Records, r)
	}
	return out, nil
}

// validRecord reports whether r is a put or use with well-formed ids.
func validRecord(r Record) bool {
	switch r.Op {
	case opPut:
		return validID([]byte(r.ActionID)) && validID([]byte(r.OutputID)) && r.Size >= 0
	case opUse:
		return validID([]byte(r.ActionID))
	}
	return false
}
