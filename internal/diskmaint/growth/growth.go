// Package growth turns a ring of disk-usage samples into a GiB-per-day rate
// and a days-left projection. Append and Read own a JSON-lines file; Rate and
// DaysLeft are pure and take the clock from their caller.
package growth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

const (
	// DefaultKeep is the ring length Append trims to when its keep is not
	// positive: 2200 samples is about seven days at one sample per five minutes.
	DefaultKeep = 2200

	// gibBytes is one GiB, the unit Rate reports in and DaysLeft divides by.
	gibBytes = 1 << 30

	// secondsPerDay turns a span counted in seconds into the days a rate is
	// quoted in.
	secondsPerDay = 24 * 60 * 60

	// maxLineBytes bounds one line's length. Append writes about 100 bytes a
	// line, so a longer line is damage; skipping it bounds what one bad line
	// costs a read.
	maxLineBytes = 64 << 10
)

var (
	// errEmptyPath refuses a blank path, which os.Open reports as a missing
	// file: Read would report a caller's unset path as a ring with no samples.
	errEmptyPath = errors.New("growth: empty path")

	// errInvalidSample refuses a reading Read would call corrupt: a zero time
	// cannot be placed in a window, and a negative byte count is not a reading.
	errInvalidSample = errors.New("growth: sample has no time or a negative byte count")
)

// Sample is one reading of a disk's held and total bytes at one time.
type Sample struct {
	At         time.Time `json:"at"`
	UsedBytes  int64     `json:"used_bytes"`
	TotalBytes int64     `json:"total_bytes"`
}

// validSample reports whether s is a reading Rate and DaysLeft can use.
func validSample(s Sample) bool {
	return !s.At.IsZero() && s.UsedBytes >= 0 && s.TotalBytes >= 0
}

// Append writes s as one JSON line to path and trims the file to its newest
// keep lines, DefaultKeep when keep is not positive. The write goes through a
// temp file and a rename, so a crash leaves the old ring whole.
func Append(path string, s Sample, keep int) error {
	if path == "" {
		return errEmptyPath
	}
	if !validSample(s) {
		return errInvalidSample
	}
	if keep <= 0 {
		keep = DefaultKeep
	}
	line, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("growth: encoding sample: %w", err)
	}
	lines, _, err := readLines(path)
	if err != nil {
		return err
	}
	if len(lines) >= keep {
		lines = lines[len(lines)-keep+1:]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("growth: creating directory for %s: %w", path, err)
	}
	buf := make([]byte, 0, len(line)+1)
	for _, l := range lines {
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	buf = append(buf, line...)
	buf = append(buf, '\n')
	if err := atomicfile.WriteFile(path, buf, 0o644); err != nil {
		return fmt.Errorf("growth: writing %s: %w", path, err)
	}
	return nil
}

// Read returns the samples stored at path, in line order. A line that does not
// parse as a Sample is skipped and a file that does not exist yet holds no
// samples; ReadCount also reports how many lines were skipped.
func Read(path string) ([]Sample, error) {
	samples, _, err := ReadCount(path)
	return samples, err
}

// ReadCount is Read plus the number of lines it skipped as corrupt.
func ReadCount(path string) ([]Sample, int, error) {
	if path == "" {
		return nil, 0, errEmptyPath
	}
	lines, corrupt, err := readLines(path)
	if err != nil {
		return nil, corrupt, err
	}
	samples := make([]Sample, 0, len(lines))
	for _, line := range lines {
		var s Sample
		if err := json.Unmarshal(line, &s); err != nil || !validSample(s) {
			corrupt++
			continue
		}
		samples = append(samples, s)
	}
	return samples, corrupt, nil
}

// readLines returns the file's lines, trimmed, and how many ran past
// maxLineBytes. A file that does not exist yet is no lines; every other open
// or read failure is returned, so a ring the reader cannot see never passes as
// an empty one.
func readLines(path string) ([][]byte, int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("growth: opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var lines [][]byte
	tooLong := 0
	br := bufio.NewReaderSize(f, maxLineBytes)
	buf := make([]byte, 0, 256)
	over := false
	for {
		chunk, rerr := br.ReadSlice('\n')
		if !over {
			if len(buf)+len(chunk) > maxLineBytes {
				over, buf = true, buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case rerr == nil:
			if over {
				tooLong++
			} else {
				lines = append(lines, bytes.Clone(bytes.TrimSpace(buf)))
			}
			over, buf = false, buf[:0]
		case errors.Is(rerr, bufio.ErrBufferFull):
			// The line runs past the read buffer; the cap above decides
			// whether it is kept.
		case errors.Is(rerr, io.EOF):
			if over {
				tooLong++
			} else if len(buf) > 0 {
				lines = append(lines, bytes.Clone(bytes.TrimSpace(buf)))
			}
			return lines, tooLong, nil
		default:
			return nil, tooLong, fmt.Errorf("growth: reading %s: %w", path, rerr)
		}
	}
}

// Rate fits the samples whose time falls in the window ending at now by least
// squares and returns the trend in GiB per day. A prune enters the fit as one
// step spread across every point, so it bends the trend rather than reading as
// the whole movement. It reports not ok for fewer than three samples in the
// window, a span covering less than half the window, or a window that is not
// positive.
func Rate(samples []Sample, now time.Time, window time.Duration) (float64, bool) {
	if window <= 0 {
		return 0, false
	}
	start := now.Add(-window)
	in := make([]Sample, 0, len(samples))
	for _, s := range samples {
		if s.At.Before(start) || s.At.After(now) {
			continue
		}
		in = append(in, s)
	}
	if len(in) < 3 {
		return 0, false
	}
	oldest, newest := in[0].At, in[0].At
	for _, s := range in[1:] {
		if s.At.Before(oldest) {
			oldest = s.At
		}
		if s.At.After(newest) {
			newest = s.At
		}
	}
	if span := newest.Sub(oldest); span <= 0 || span < window/2 {
		return 0, false
	}
	// x is days from the oldest sample and y is used bytes, both centered on
	// their means so each product is trend-sized rather than reading-sized and
	// the sums keep float64's precision. span > 0 above means two x differ, so
	// den is positive.
	var n, sumX, sumY float64
	for _, s := range in {
		sumX += s.At.Sub(oldest).Seconds() / secondsPerDay
		sumY += float64(s.UsedBytes)
		n++
	}
	meanX, meanY := sumX/n, sumY/n
	var num, den float64
	for _, s := range in {
		dx := s.At.Sub(oldest).Seconds()/secondsPerDay - meanX
		num += dx * (float64(s.UsedBytes) - meanY)
		den += dx * dx
	}
	return num / den / gibBytes, true
}

// DaysLeft projects how many days pass before used reaches total at gibPerDay.
// It reports not ok when the rate is not a positive number, or when total is
// not a positive capacity or used is negative: those are not a projection, and
// a caller must not read one as a disk filling on schedule. A disk already at
// or over its total has 0 days left, which is a real answer.
func DaysLeft(total, used int64, gibPerDay float64) (float64, bool) {
	// Written as a negated comparison so a NaN rate refuses as well.
	if !(gibPerDay > 0) {
		return 0, false
	}
	if total <= 0 || used < 0 {
		return 0, false
	}
	remaining := total - used
	if remaining <= 0 {
		return 0, true
	}
	return float64(remaining) / (gibPerDay * gibBytes), true
}
