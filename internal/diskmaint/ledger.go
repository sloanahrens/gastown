package diskmaint

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"time"
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
// only state diskmaint keeps.
type Ledger struct {
	path     string
	maxBytes int64
	write    writeFunc
}

// writeFunc is the write one append makes; a test hands the ledger one that
// fails, since a full disk cannot be arranged hermetically.
type writeFunc func(f *os.File, b []byte) (int, error)

func osWrite(f *os.File, b []byte) (int, error) { return f.Write(b) }

// NewLedger returns a ledger at path that rotates to path+".1" instead of
// letting the live file grow past maxBytes. A maxBytes of 0 or less leaves
// rotation off.
func NewLedger(path string, maxBytes int64) *Ledger {
	return &Ledger{path: path, maxBytes: maxBytes, write: osWrite}
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
	if err := l.rotateIfNeeded(int64(len(line))); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := l.write(f, line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// rotateIfNeeded renames a live file that an add of n more bytes would take
// past maxBytes. A missing file has nothing to rotate.
func (l *Ledger) rotateIfNeeded(n int64) error {
	if l.maxBytes <= 0 {
		return nil
	}
	info, err := os.Stat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size()+n <= l.maxBytes {
		return nil
	}
	return os.Rename(l.path, l.path+".1")
}

// Tail returns up to the newest n entries oldest-first, reading path+".1" as
// well when the live file does not hold enough. A missing file has no
// entries; a line that does not decode is skipped, so a torn write costs that
// one line and not the runs before it.
func (l *Ledger) Tail(n int) ([]LedgerEntry, error) {
	if n <= 0 {
		return nil, nil
	}
	older, err := readEntries(l.path + ".1")
	if err != nil {
		return nil, err
	}
	live, err := readEntries(l.path)
	if err != nil {
		return nil, err
	}
	all := append(older, live...)
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

func readEntries(path string) ([]LedgerEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []LedgerEntry
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e LedgerEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
