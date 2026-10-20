package dedupe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// Line is one line of the suggestion log: what one finding was offered and
// what Jev answered. It holds bead IDs and numbers only, never a title, a
// description, a prompt or key text.
type Line struct {
	// TS is when the answer was logged.
	TS time.Time `json:"ts"`
	// Finding is the bead the line is about.
	Finding string `json:"finding"`
	// Candidate is the bead Jev chose as the duplicate, or "none".
	Candidate string `json:"candidate"`
	// Confidence is Jev's confidence in its answer, 0 when no answer came.
	Confidence float64 `json:"confidence"`
	// CandidatesOffered lists the beads the question offered, in the order
	// the question gave them.
	CandidatesOffered []string `json:"candidates_offered"`
}

// LogPath is the suggestion log, one JSON line per finding.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "dedupe.jsonl")
}

// CursorPath is the file that remembers the last finding a pass asked about.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "dedupe.cursor.json")
}

// cursor is the last finding asked about: its creation time, and its ID to
// break a tie on that time.
type cursor struct {
	Time time.Time `json:"time"`
	ID   string    `json:"id,omitempty"`
}

// appendLine appends one line to the log, creating the directory 0700 and
// the file 0600.
func appendLine(path string, l Line) error {
	line, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encoding the dedupe log line: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the dedupe log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening the dedupe log: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the dedupe log: %w", err)
	}
	return f.Close()
}

// ReadRecent returns the last n lines of the log, oldest first. A missing log
// is empty, not an error, and a line that does not parse is skipped.
func ReadRecent(path string, n int) ([]Line, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening the dedupe log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var lines []Line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var l Line
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			continue
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the dedupe log: %w", err)
	}
	if n < len(lines) {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// readCursor returns the saved cursor, and false when there is none yet.
func readCursor(path string) (cursor, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cursor{}, false, nil
	}
	if err != nil {
		return cursor{}, false, fmt.Errorf("reading the dedupe cursor: %w", err)
	}
	var c cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return cursor{}, false, fmt.Errorf("parsing the dedupe cursor %s: %w", path, err)
	}
	return c, true, nil
}

// writeCursor saves the cursor through a temporary file, so a crash never
// leaves a half-written cursor that would make a pass ask twice.
func writeCursor(path string, c cursor) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding the dedupe cursor: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the dedupe cursor directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing the dedupe cursor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving the dedupe cursor: %w", err)
	}
	return nil
}

// waitFor pauses for d, or until ctx is done.
func waitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
