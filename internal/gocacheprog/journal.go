package gocacheprog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// Record kinds: the start line opens a journal, put and use follow it.
const (
	opStart = "start"
	opPut   = "put"
	opUse   = "use"
)

// Journal is one process's record of the entries it stored and used. Its file
// stays locked until Close, so a reader that finds the lock held knows the
// process may still use the entries the journal names.
type Journal struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	unlock func()
	closed bool
}

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

// openJournalAt creates the journal file at path, locks it and writes its
// start line stamped with now. A failure after the file is created releases
// the lock and closes the file.
func openJournalAt(path string, now func() time.Time) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("gocacheprog: journal %s: open: %w", path, err)
	}
	unlock, ok, err := lock.FlockTryAcquire(path)
	if err == nil && !ok {
		err = lock.ErrFlockHeld
	}
	j := &Journal{path: path, f: f, unlock: unlock}
	if err == nil {
		err = j.write(Record{Op: opStart, Time: now()})
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("gocacheprog: journal %s: %w", path, err), j.Close())
	}
	return j, nil
}

// Path returns the file this journal writes.
func (j *Journal) Path() string { return j.path }

// Close releases the lock and closes the file. A second Close returns nil.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	err := j.f.Close()
	if j.unlock != nil {
		j.unlock()
	}
	if err != nil {
		return fmt.Errorf("gocacheprog: journal %s: close: %w", j.path, err)
	}
	return nil
}

// write appends r as one JSON line in a single Write call. After Close the
// write fails, because the file is closed.
func (j *Journal) write(r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("gocacheprog: journal %s: encode: %w", j.path, err)
	}
	if _, err := j.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("gocacheprog: journal %s: write: %w", j.path, err)
	}
	return nil
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
