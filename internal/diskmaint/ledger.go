package diskmaint

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// LedgerEntry is one run's record.
type LedgerEntry struct {
	TS         time.Time `json:"ts"`
	Target     string    `json:"target"`
	Mode       string    `json:"mode"` // "dry-run" | "apply"
	Planned    int       `json:"planned"`
	Done       int       `json:"done"`
	BytesFreed int64     `json:"bytes_freed"`
	Refused    string    `json:"refused,omitempty"`
	Deferred   string    `json:"deferred,omitempty"`
	Errors     []string  `json:"errors,omitempty"`
}

// Ledger is an append-only JSON-lines record of maintenance runs; it is the
// only state diskmaint keeps. Appends and reads take one exclusive lock beside
// the ledger, so a rotation and an append that race cannot lose an entry.
type Ledger struct {
	path     string
	maxBytes int64
	write    writeFunc
	open     openFunc
	acquire  lockFunc

	mu      sync.Mutex
	skipped int
}

// writeFunc is the write one append makes; a test hands the ledger one that
// fails, since a full disk cannot be arranged hermetically.
type writeFunc func(f *os.File, b []byte) (int, error)

// openFunc opens the live file for an append. It is a field so a test can fail
// the open, which a file the process may read but not write cannot do without
// depending on who the test runs as.
type openFunc func(path string) (*os.File, error)

// lockFunc takes the exclusive cross-process lock at path and returns its
// release. It is a field so a test can see the lock around a write and hand the
// ledger an acquisition that fails.
type lockFunc func(path string) (func(), error)

func osWrite(f *os.File, b []byte) (int, error) { return f.Write(b) }

func osOpenAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// NewLedger returns a ledger at path that rotates to path+".1" instead of
// letting the live file grow past maxBytes. A maxBytes of 0 or less leaves
// rotation off.
func NewLedger(path string, maxBytes int64) *Ledger {
	return &Ledger{path: path, maxBytes: maxBytes, write: osWrite, open: osOpenAppend, acquire: lock.FlockAcquire}
}

// Append writes one entry as a JSON line. An entry that would push the live
// file past maxBytes rotates it to path+".1" first, discarding an older
// rotation.
func (l *Ledger) Append(e LedgerEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	unlock, err := l.acquire(l.lockPath())
	if err != nil {
		return err
	}
	defer unlock()
	return l.appendLocked(line)
}

// appendLocked writes one line to the live file while Append's lock is held.
func (l *Ledger) appendLocked(line []byte) error {
	size, torn, err := l.tailState()
	if err != nil {
		return err
	}
	// A writer that died mid-line left a record without its newline. Writing
	// straight after it would glue the new entry onto the torn text and lose
	// both, so start a line instead; the fragment stays torn and is skipped on
	// read. A record that lost only its newline becomes readable again.
	lead := 0
	if torn {
		lead = 1
	}
	// A live file of size 0 never rotates: that is the file a rotation just
	// made, and an entry larger than maxBytes is written rather than rotating
	// an empty file forever.
	if l.maxBytes > 0 && size > 0 && size+int64(len(line)+lead) > l.maxBytes {
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return err
		}
	} else if torn {
		line = append([]byte{'\n'}, line...)
	}
	f, err := l.open(l.path)
	if err != nil {
		return err
	}
	if _, err := l.write(f, line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// tailState reports the live file's size and whether it ends mid-line. A
// missing file has size 0 and no torn line. The live file is capped by
// maxBytes, so reading it to find the last byte costs no more than a Tail.
func (l *Ledger) tailState() (size int64, torn bool, err error) {
	data, err := os.ReadFile(l.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, false, err
	}
	if len(data) == 0 {
		return 0, false, nil
	}
	return int64(len(data)), data[len(data)-1] != '\n', nil
}

// Tail returns up to the newest n entries oldest-first, reading path+".1" as
// well when the live file does not hold enough. A missing file has no entries;
// a line that does not decode is skipped, so a torn write costs that one line
// and not the runs before it. Skipped reports how many lines that was.
func (l *Ledger) Tail(n int) ([]LedgerEntry, error) {
	if n <= 0 {
		l.setSkipped(0)
		return nil, nil
	}
	unlock, err := l.acquire(l.lockPath())
	if err != nil {
		return nil, err
	}
	defer unlock()

	older, olderSkipped, err := readEntries(l.path + ".1")
	if err != nil {
		return nil, err
	}
	live, liveSkipped, err := readEntries(l.path)
	if err != nil {
		return nil, err
	}
	l.setSkipped(olderSkipped + liveSkipped)
	all := append(older, live...)
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// Skipped reports how many lines the most recent Tail could not decode. One is
// the torn final line a crash leaves; more means the ledger is damaged, and a
// caller that reads it for a decision says so rather than report a shorter
// history as complete.
func (l *Ledger) Skipped() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.skipped
}

func (l *Ledger) setSkipped(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.skipped = n
}

// lockPath is the file whose flock serializes this ledger's writers and
// readers; it sits beside the live file so both stay in one directory.
func (l *Ledger) lockPath() string { return l.path + ".lock" }

// readEntries decodes the entries in one file and counts the lines it could
// not decode.
func readEntries(path string) ([]LedgerEntry, int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var out []LedgerEntry
	skipped := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e LedgerEntry
		if err := json.Unmarshal(line, &e); err != nil {
			skipped++
			continue
		}
		out = append(out, e)
	}
	return out, skipped, nil
}
