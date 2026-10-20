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
	"github.com/steveyegge/gastown/internal/lock"
)

// Outcome says how a line's candidate came about. It is one of a fixed set of
// codes, never free text, so it tells a real "none" from a failure without the
// log holding an error message.
type Outcome string

const (
	// OutcomeAnswered: Jev answered with an offered option (or none) at or
	// above the confidence floor. Candidate is its choice.
	OutcomeAnswered Outcome = "answered"
	// OutcomeNoCandidates: the search found nothing to offer, so Jev was not
	// asked.
	OutcomeNoCandidates Outcome = "no_candidates"
	// OutcomeSearchFailed: the hm search failed, so Jev was not asked.
	OutcomeSearchFailed Outcome = "search_failed"
	// OutcomeAskFailed: the Jev call failed (error, timeout, 429 after the
	// retry, or an unparseable reply).
	OutcomeAskFailed Outcome = "ask_failed"
	// OutcomeLowConfidence: Jev answered below the confidence floor.
	OutcomeLowConfidence Outcome = "low_confidence"
	// OutcomeUnknownOption: Jev named an option the question did not offer.
	OutcomeUnknownOption Outcome = "unknown_option"
)

// Line is one line of the suggestion log: what one finding was offered and
// what Jev answered. It holds bead IDs, numbers and an outcome code only,
// never a title, a description, a prompt, an error message or key text.
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
	// Outcome tells an answered "none" from a failure that also logs none.
	Outcome Outcome `json:"outcome"`
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

// ErrBusy means another pass holds the run lock: a manual pass and the
// daemon's pass share one cursor, so they take turns.
var ErrBusy = errors.New("another jev dedupe pass is running")

// lockRun takes the pass's run lock without waiting, and returns the release.
// The lock sits beside the cursor, so every pass over one cursor contends on
// it, in this process and in others.
func lockRun(cursorPath string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(cursorPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating the dedupe lock directory: %w", err)
	}
	release, ok, err := lock.FlockTryAcquire(cursorPath + ".lock")
	if err != nil {
		return nil, fmt.Errorf("locking the dedupe pass: %w", err)
	}
	if !ok {
		return nil, ErrBusy
	}
	return release, nil
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
