package events

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gofrs/flock"
)

// PruneTempSuffix is the suffix Prune appends to the events path while it
// builds the replacement, before renaming it into place. Exported so the
// hermetic tripwire can recognize the temp by the producer's own constant.
const PruneTempSuffix = ".prune.tmp"

// ErrPruneLockBusy means Prune gave up waiting for the writers' lock. Nothing
// was changed; the next run tries again.
var ErrPruneLockBusy = errors.New("events file lock busy")

// PruneOptions bounds what Prune keeps.
type PruneOptions struct {
	// MaxAge drops leading lines whose ts is older than now-MaxAge. Zero
	// disables the age bound.
	MaxAge time.Duration
	// MaxBytes caps the file. When the age bound leaves more than MaxBytes,
	// only the newest MaxBytes/2 is kept, so a file at the cap is not
	// rewritten on every run. Zero disables the size bound.
	MaxBytes int64
	// LockTimeout bounds the wait for the writers' lock. Zero means 10s.
	LockTimeout time.Duration
}

// PruneResult reports what one Prune did.
type PruneResult struct {
	BytesBefore  int64
	BytesAfter   int64
	LinesDropped int
}

// Pruned reports whether Prune rewrote the file.
func (r PruneResult) Pruned() bool { return r.BytesAfter < r.BytesBefore }

// Prune drops the oldest lines of the events log at path, keeping what opts
// allows, and returns what it did. A missing file is not an error.
//
// It holds the writers' flock (path + ".lock", the lock writeTo takes) for the
// whole rewrite, so no append can land between the copy and the rename. Lines
// are only ever dropped from the front, cuts fall on line boundaries, and the
// kept bytes are copied verbatim, so the newest line and any partial line a
// crashed writer left are preserved exactly. The replacement is renamed over
// path: events.Tail follows the rename and resumes after its anchor
// (claude-9jq), and writers open the path fresh for every append.
func Prune(path string, opts PruneOptions, now time.Time) (PruneResult, error) {
	timeout := opts.LockTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fl := flock.New(path + ".lock")
	locked, err := fl.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return PruneResult{}, fmt.Errorf("acquiring events file lock: %w", err)
	}
	if !locked {
		return PruneResult{}, ErrPruneLockBusy
	}
	defer fl.Unlock() //nolint:errcheck // best-effort unlock

	f, err := os.Open(path) //nolint:gosec // G304: the town's own events log
	if err != nil {
		if os.IsNotExist(err) {
			return PruneResult{}, nil
		}
		return PruneResult{}, fmt.Errorf("opening events file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return PruneResult{}, fmt.Errorf("stat events file: %w", err)
	}
	size := info.Size()
	result := PruneResult{BytesBefore: size, BytesAfter: size}

	cut, dropped, err := pruneCut(f, size, opts, now)
	if err != nil || cut == 0 {
		return result, err
	}

	if err := replaceTail(path, f, cut, info.Mode().Perm()); err != nil {
		return result, err
	}
	result.BytesAfter = size - cut
	result.LinesDropped = dropped
	return result, nil
}

// pruneCut returns the offset of the first byte to keep and how many lines
// lie before it.
func pruneCut(f *os.File, size int64, opts PruneOptions, now time.Time) (int64, int, error) {
	var cut int64
	dropped := 0
	if opts.MaxAge > 0 {
		cutoff := now.Add(-opts.MaxAge)
		r := bufio.NewReader(f)
		var pos int64
		lines := 0
		for {
			line, err := r.ReadBytes('\n')
			if err == io.EOF {
				break // a partial last line is never dropped for age
			}
			if err != nil {
				return 0, 0, fmt.Errorf("reading events file: %w", err)
			}
			pos += int64(len(line))
			lines++
			ts, ok := lineTime(line)
			if !ok {
				// No timestamp: drop it only if an old line follows.
				continue
			}
			if !ts.Before(cutoff) {
				break
			}
			cut, dropped = pos, lines
		}
	}

	if opts.MaxBytes > 0 && size-cut > opts.MaxBytes {
		boundary, n, err := lineBoundaryAfter(f, cut, size-opts.MaxBytes/2)
		if err != nil {
			return 0, 0, err
		}
		cut, dropped = boundary, dropped+n
	}
	return cut, dropped, nil
}

// lineTime parses the ts field of one events line.
func lineTime(line []byte) (time.Time, bool) {
	var ev struct {
		Timestamp string `json:"ts"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Timestamp == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, ev.Timestamp)
	return ts, err == nil
}

// lineBoundaryAfter returns the offset just past the first newline at or
// after target (scanning from from), and the number of lines in [from, that
// offset). With no newline at or after target it returns from and 0: a lone
// oversized partial line is left alone rather than cut mid-line.
func lineBoundaryAfter(f *os.File, from, target int64) (int64, int, error) {
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, 0, fmt.Errorf("seeking events file: %w", err)
	}
	r := bufio.NewReader(f)
	pos, lines := from, 0
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return from, 0, nil
		}
		if err != nil {
			return 0, 0, fmt.Errorf("reading events file: %w", err)
		}
		pos += int64(len(line))
		lines++
		if pos > target {
			return pos, lines, nil
		}
	}
}

// replaceTail atomically replaces path with the bytes of f from cut to EOF.
// A crash leaves path + PruneTempSuffix behind; the next prune removes it.
func replaceTail(path string, f *os.File, cut int64, perm os.FileMode) (err error) {
	tmpPath := path + PruneTempSuffix
	if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale prune temp: %w", err)
	}
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm) //nolint:gosec // G304: sibling of the town's own events log
	if err != nil {
		return fmt.Errorf("creating prune temp: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err = f.Seek(cut, io.SeekStart); err != nil {
		return fmt.Errorf("seeking events file: %w", err)
	}
	if _, err = io.Copy(tmp, f); err != nil {
		return fmt.Errorf("copying kept events: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("syncing prune temp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing prune temp: %w", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing events file: %w", err)
	}
	return nil
}
