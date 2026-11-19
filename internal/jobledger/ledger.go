// Package jobledger records each maintenance job run in an append-only
// JSON-lines ledger under <town>/.runtime/jobs, so a job that only logs a line
// can say what it did and when it is next due.
package jobledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

const (
	// MaxStats caps the Stats keys one run may carry.
	MaxStats = 16
	// maxStatName caps one Stats key, in bytes.
	maxStatName = 32
	// MaxBytes is the size the live file may reach before Append rotates it.
	MaxBytes = 1 << 20
	// readAttempts bounds how often a read repeats when a rotation lands between its reads.
	readAttempts = 3

	// liveName and olderName are the ledger's files, relative to the town root.
	liveName  = ".runtime/jobs/runs.jsonl"
	olderName = liveName + ".1"
)

// Run is one run of a maintenance job.
type Run struct {
	Job       string           `json:"job"`
	StartedAt time.Time        `json:"started_at"`
	EndedAt   time.Time        `json:"ended_at"`
	Outcome   string           `json:"outcome"`
	Stats     map[string]int64 `json:"stats,omitempty"`
	NextDue   *time.Time       `json:"next_due,omitempty"`
}

// Ledger is the run ledger of one town. Append takes an exclusive lock beside
// the live file. Last and Tail take no lock and write nothing, because the
// dashboard reads the ledger from a read-only mount.
type Ledger struct {
	path     string
	maxBytes int64
	readFS   fs.FS
	open     openFunc
	statOpen statFunc
	write    writeFunc
	acquire  lockFunc

	mu      sync.Mutex
	skipped int
}

// openFunc opens the live file for an append. It is a field so a test can fail
// the open, which a file the process may read but not write cannot do.
type openFunc func(path string) (*os.File, error)

// statFunc stats an open live file. It is a field so a test can fail the stat,
// which a healthy file never does.
type statFunc func(f *os.File) (fs.FileInfo, error)

// writeFunc is the write one append makes; a test hands the ledger one that
// fails or writes short, since a full disk cannot be arranged hermetically.
type writeFunc func(f *os.File, b []byte) (int, error)

// lockFunc takes the exclusive cross-process lock at path and returns its release.
type lockFunc func(path string) (func(), error)

// fileState is the size and mtime of one ledger file; the zero value is a missing file.
type fileState struct {
	size int64
	mod  int64
}

// NewLedger returns the run ledger of the town at townRoot. The directory is
// created by the first Append.
func NewLedger(townRoot string) (*Ledger, error) {
	if townRoot == "" {
		return nil, errors.New("jobledger: town root is empty")
	}
	return &Ledger{
		path:     filepath.Join(townRoot, filepath.FromSlash(liveName)),
		maxBytes: MaxBytes,
		readFS:   os.DirFS(townRoot),
		open:     osOpenAppend,
		statOpen: (*os.File).Stat,
		write:    osWrite,
		acquire:  lock.FlockAcquire,
	}, nil
}

func osOpenAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func osWrite(f *os.File, b []byte) (int, error) { return f.Write(b) }

// Append writes one run as a JSON line. A run that would push the live file past
// MaxBytes rotates it to runs.jsonl.1 first, discarding the older rotation.
func (l *Ledger) Append(r Run) error {
	if err := checkJob(r.Job); err != nil {
		return err
	}
	if err := checkStats(r); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("jobledger: encode run of %s: %w", r.Job, err)
	}
	line = append(line, '\n')

	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("jobledger: create %s: %w", dir, err)
	}
	unlock, err := l.acquire(l.path + ".lock")
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
	if l.maxBytes > 0 && size > 0 && size+int64(len(line)) > l.maxBytes {
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return fmt.Errorf("jobledger: rotate %s: %w", l.path, err)
		}
	} else if torn {
		// A writer that died mid-line left a fragment without its newline. Start a
		// line instead of gluing the new run onto it; the fragment is skipped on read.
		line = append([]byte{'\n'}, line...)
	}
	f, err := l.open(l.path)
	if err != nil {
		return fmt.Errorf("jobledger: open %s: %w", l.path, err)
	}
	n, err := l.write(f, line)
	if err != nil {
		f.Close()
		return fmt.Errorf("jobledger: write %s: %w", l.path, err)
	}
	if n != len(line) {
		f.Close()
		return fmt.Errorf("jobledger: write %s: %w", l.path, io.ErrShortWrite)
	}
	return f.Close()
}

// tailState reports the live file's size and whether it ends mid-line. A missing
// file has size 0 and no torn line. It reads only the last byte.
func (l *Ledger) tailState() (size int64, torn bool, err error) {
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("jobledger: open %s: %w", l.path, err)
	}
	defer f.Close()
	info, err := l.statOpen(f)
	if err != nil {
		return 0, false, fmt.Errorf("jobledger: stat %s: %w", l.path, err)
	}
	if info.Size() == 0 {
		return 0, false, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
		return 0, false, fmt.Errorf("jobledger: read %s: %w", l.path, err)
	}
	return info.Size(), last[0] != '\n', nil
}

// Last returns the newest run of job. The bool is false when job has no run.
func (l *Ledger) Last(job string) (Run, bool, error) {
	runs, err := l.Tail(job, 1)
	if err != nil || len(runs) == 0 {
		return Run{}, false, err
	}
	return runs[0], true, nil
}

// Tail returns up to the newest n runs of job, oldest first. A ledger that does
// not exist yet has no runs and no error. Lines that do not decode are skipped
// and counted; Skipped reports the count of the latest call.
func (l *Ledger) Tail(job string, n int) ([]Run, error) {
	l.setSkipped(0)
	if err := checkJob(job); err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, nil
	}
	runs, skipped, err := l.history(job)
	if err != nil {
		return nil, err
	}
	l.setSkipped(skipped)
	if len(runs) > n {
		runs = runs[len(runs)-n:]
	}
	return runs, nil
}

// Skipped reports how many lines the latest Last or Tail could not decode. One
// is the torn final line a crash leaves; more means the ledger is damaged.
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

// history reads the runs of job from the rotation and the live file. Readers take
// no lock, so a rotation can land between the two reads and move runs out of
// the file read second. The read repeats until both files hold still across it.
// The stat check samples size and mtime, not identity: a rotation that leaves
// both files with the same size and mtime would pass it.
func (l *Ledger) history(job string) ([]Run, int, error) {
	for range readAttempts {
		before, err := l.snapshot()
		if err != nil {
			return nil, 0, err
		}
		older, olderSkipped, err := l.readEntries(olderName)
		if err != nil {
			return nil, 0, err
		}
		live, liveSkipped, err := l.readEntries(liveName)
		if err != nil {
			return nil, 0, err
		}
		after, err := l.snapshot()
		if err != nil {
			return nil, 0, err
		}
		if before != after {
			continue
		}
		var out []Run
		for _, r := range append(older, live...) {
			if r.Job == job {
				out = append(out, r)
			}
		}
		return out, olderSkipped + liveSkipped, nil
	}
	return nil, 0, fmt.Errorf("jobledger: ledger rotated during each of %d reads; retry", readAttempts)
}

// snapshot stats both ledger files. A missing file has the zero fileState.
func (l *Ledger) snapshot() ([2]fileState, error) {
	var s [2]fileState
	for i, name := range []string{olderName, liveName} {
		info, err := fs.Stat(l.readFS, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return s, fmt.Errorf("jobledger: stat %s: %w", name, err)
		}
		s[i] = fileState{size: info.Size(), mod: info.ModTime().UnixNano()}
	}
	return s, nil
}

// readEntries decodes the runs in one ledger file and counts the lines it could
// not decode. A job-less line is one that cannot be attributed, so it is skipped too.
func (l *Ledger) readEntries(name string) ([]Run, int, error) {
	data, err := fs.ReadFile(l.readFS, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("jobledger: read %s: %w", name, err)
	}
	var out []Run
	skipped := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r Run
		if err := json.Unmarshal(line, &r); err != nil || r.Job == "" {
			skipped++
			continue
		}
		out = append(out, r)
	}
	return out, skipped, nil
}

// checkJob refuses a job name that is empty or padded with space. Append writes
// only names that pass it, so a read with a padded name could never match a run.
func checkJob(job string) error {
	if strings.TrimSpace(job) == "" || strings.TrimSpace(job) != job {
		return fmt.Errorf("jobledger: job name %q must be non-empty and not padded with space", job)
	}
	return nil
}

// checkStats refuses a run with more than MaxStats keys, or a key that is empty,
// padded with space or longer than maxStatName bytes.
func checkStats(r Run) error {
	if len(r.Stats) > MaxStats {
		return fmt.Errorf("jobledger: run of %s has %d stats; the cap is %d", r.Job, len(r.Stats), MaxStats)
	}
	for name := range r.Stats {
		if name == "" || strings.TrimSpace(name) != name || len(name) > maxStatName {
			return fmt.Errorf("jobledger: run of %s has stat name %q; names are non-empty, trimmed and at most %d bytes", r.Job, name, maxStatName)
		}
	}
	return nil
}
