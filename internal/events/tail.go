package events

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
)

// tailAnchorScan bounds how far back OpenTail looks for the last complete line.
const tailAnchorScan int64 = 64 << 10

// tailAnchorDepth is how many recently passed lines are kept as anchors. More
// than one matters when the newest line never reached the replacement file
// (an unlocked writer appended to the old inode after the rotator copied it).
const tailAnchorDepth = 8

// Tail follows a line-oriented file by path, not by file descriptor.
//
// A plain tail keeps reading the descriptor it opened. When the file is
// replaced (tmp + rename, as the KRC pruner does on every daemon start and
// hourly) the descriptor keeps pointing at the old inode, which no writer ever
// touches again, so the tail goes deaf. That was the 19:42 "missed events"
// incident (claude-9jq).
//
// Poll detects two kinds of rotation:
//   - replacement: the path now names a different file (os.SameFile compares
//     device and inode on darwin and linux). The old file is drained first,
//     then the new one is opened.
//   - truncation in place: same file, but smaller than the read offset.
//
// A rotated file usually starts with history the rotator kept. To avoid
// replaying it, the tail remembers the last complete line it has passed (the
// anchor) and resumes after the anchor's last occurrence in the new content.
// It keeps the last few lines as anchors and uses the newest one present. If
// none is there, it reads the new content from the start: a spurious wake is
// recoverable, a missed event is not.
//
// Limitation: a truncate-in-place that regrows past the read offset between
// two polls is indistinguishable from appends and is not detected.
type Tail struct {
	path    string
	f       *os.File
	r       *bufio.Reader
	pos     int64    // bytes of f consumed through r
	partial string   // bytes of an unfinished line
	anchors []string // recent complete non-empty lines at or before pos, newest last
}

// OpenTail opens path (creating it if missing) positioned at its end, so only
// lines appended after the call are returned.
func OpenTail(path string) (*Tail, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0644) //nolint:gosec // G302/G304: events file is non-sensitive operational data
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seeking to end of %s: %w", path, err)
	}
	anchor, err := lastCompleteLine(f, end)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seeking to end of %s: %w", path, err)
	}
	t := &Tail{path: path, f: f, r: bufio.NewReader(f), pos: end}
	if anchor != "" {
		t.anchors = []string{anchor}
	}
	return t, nil
}

// Close releases the open file.
func (t *Tail) Close() error {
	return t.f.Close()
}

// Poll returns every complete line appended since the previous call, without
// trailing newlines, following the path across a rotation.
func (t *Tail) Poll() ([]string, error) {
	lines, err := t.drain()
	if err != nil {
		return lines, err
	}
	switched, more := t.followRotation()
	lines = append(lines, more...)
	if !switched {
		return lines, nil
	}
	fresh, err := t.drain()
	return append(lines, fresh...), err
}

// drain reads every complete line currently available from f.
func (t *Tail) drain() ([]string, error) {
	var lines []string
	for {
		chunk, err := t.r.ReadString('\n')
		t.pos += int64(len(chunk))
		t.partial += chunk
		if strings.HasSuffix(t.partial, "\n") {
			line := strings.TrimSuffix(t.partial, "\n")
			t.partial = ""
			if line != "" {
				t.pushAnchor(line)
				lines = append(lines, line)
			}
		}
		if err == io.EOF {
			return lines, nil
		}
		if err != nil {
			return lines, fmt.Errorf("reading %s: %w", t.path, err)
		}
	}
}

func (t *Tail) pushAnchor(line string) {
	t.anchors = append(t.anchors, line)
	if len(t.anchors) > tailAnchorDepth {
		t.anchors = t.anchors[len(t.anchors)-tailAnchorDepth:]
	}
}

// followRotation switches to the file now at t.path when the current one was
// replaced or truncated. It returns whether it repositioned and any lines that
// were still unread in the old file. Transient stat/open failures leave the
// tail on its current file; the next poll retries.
func (t *Tail) followRotation() (bool, []string) {
	pathInfo, err := os.Stat(t.path)
	if err != nil {
		return false, nil
	}
	fdInfo, err := t.f.Stat()
	if err != nil {
		return false, nil
	}

	if os.SameFile(pathInfo, fdInfo) {
		if fdInfo.Size() >= t.pos {
			return false, nil
		}
		// Truncated in place.
		if err := t.resumeAfterAnchor(t.f); err != nil {
			return false, nil
		}
		return true, nil
	}

	// Replaced. A writer that opened the old file before the rename may have
	// appended since the last drain; collect that before letting go of it.
	late, _ := t.drain()

	nf, err := os.Open(t.path)
	if err != nil {
		return false, late
	}
	old := t.f
	if err := t.resumeAfterAnchor(nf); err != nil {
		_ = nf.Close()
		return false, late
	}
	_ = old.Close()
	return true, late
}

// resumeAfterAnchor makes f the tail's file, positioned just after the last
// occurrence of the newest anchor it contains, or at the start when it
// contains none.
func (t *Tail) resumeAfterAnchor(f *os.File) error {
	off, err := offsetAfterAnchors(f, t.anchors)
	if err != nil {
		return err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	t.f = f
	t.r = bufio.NewReader(f)
	t.pos = off
	t.partial = ""
	return nil
}

// offsetAfterAnchors returns the offset just past the last occurrence of the
// newest anchor (anchors are ordered oldest first) found as a complete line in
// f, or 0 when none is found.
func offsetAfterAnchors(f *os.File, anchors []string) (int64, error) {
	if len(anchors) == 0 {
		return 0, nil
	}
	want := make(map[string]int64, len(anchors))
	for _, a := range anchors {
		want[a] = -1
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	r := bufio.NewReader(f)
	var pos int64
	for {
		line, err := r.ReadString('\n')
		pos += int64(len(line))
		if strings.HasSuffix(line, "\n") {
			if _, ok := want[strings.TrimSuffix(line, "\n")]; ok {
				want[strings.TrimSuffix(line, "\n")] = pos
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	for i := len(anchors) - 1; i >= 0; i-- {
		if off := want[anchors[i]]; off >= 0 {
			return off, nil
		}
	}
	return 0, nil
}

// lastCompleteLine returns the last newline-terminated, non-empty line ending
// at or before end, or "" when none fits in the scan window.
func lastCompleteLine(f *os.File, end int64) (string, error) {
	start := end - tailAnchorScan
	if start < 0 {
		start = 0
	}
	buf := make([]byte, end-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return "", fmt.Errorf("reading tail of file: %w", err)
	}
	for {
		last := bytes.LastIndexByte(buf, '\n')
		if last < 0 {
			return "", nil
		}
		buf = buf[:last]
		prev := bytes.LastIndexByte(buf, '\n')
		if prev < 0 && start > 0 {
			return "", nil // line began before the scan window
		}
		line := buf[prev+1:]
		if len(line) > 0 {
			return string(line), nil
		}
	}
}
