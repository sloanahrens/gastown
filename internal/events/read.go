package events

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Read returns the town's events log in append order.
//
// A town that has never logged an event returns no events and no error, so a
// reader can treat "nothing happened yet" and "no log yet" the same way.
// Lines that are not events are skipped rather than reported: the last line of
// a file a writer is appending to is routinely incomplete mid-write, and the
// events log is a best-effort record, not a database. The cost is that a
// genuinely corrupt line is invisible here — internal/doctor's test-leak check
// is what reports those.
func Read(townRoot string) ([]Event, error) {
	events, _, err := ReadTail(townRoot, 0)
	return events, err
}

// ReadTail returns the town's events log in append order, reading at most the
// last maxBytes of the file, and whether it read the file whole. maxBytes at or
// below zero reads the whole file.
//
// The bounded read is for a reader that needs a recent answer and must not pay
// for the whole log — a landing that asks why a bead was dispatched, say. A
// bounded read that started inside the file cannot be trusted to hold the start
// of what the caller looks for, so it reports whole false rather than let the
// caller read a tail as if it were the log; the partial line the window starts
// in is dropped. A missing log is no events and no error (Read).
func ReadTail(townRoot string, maxBytes int64) (events []Event, whole bool, err error) {
	if townRoot == "" {
		return nil, true, nil
	}

	f, err := os.Open(filepath.Join(townRoot, EventsFile)) //nolint:gosec // G304: path is under the town root
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true, nil
		}
		return nil, true, fmt.Errorf("opening events file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	info, err := f.Stat()
	if err != nil {
		return nil, true, fmt.Errorf("measuring events file: %w", err)
	}
	if maxBytes <= 0 || info.Size() <= maxBytes {
		events, err = readFrom(f, false)
		return events, true, err
	}
	start := info.Size() - maxBytes
	// A window that begins mid-line loses that line: it is the tail of one the
	// window cut, and half an event is not an event.
	prev := []byte{' '}
	if _, err := f.ReadAt(prev, start-1); err != nil {
		return nil, false, fmt.Errorf("reading the events file at the window's start: %w", err)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, false, fmt.Errorf("seeking to the tail of the events file: %w", err)
	}
	events, err = readFrom(f, prev[0] != '\n')
	return events, false, err
}

// readFrom decodes every event from f's current position to its end, dropping
// the first line when skipFirst: a line the caller's window cut in half.
func readFrom(f *os.File, skipFirst bool) ([]Event, error) {
	scanner := bufio.NewScanner(f)
	// Event lines carry message bodies, so the default 64KiB token is not
	// enough for a long nudge. 1MiB is the ceiling the daemon's prune shares.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if skipFirst {
		scanner.Scan()
	}
	var events []Event
	for scanner.Scan() {
		if event, ok := decodeEventLine(scanner.Text()); ok {
			events = append(events, event)
		}
	}
	if err := scanner.Err(); err != nil {
		// Events already read are still returned: a partial view beats none.
		return events, fmt.Errorf("reading events file: %w", err)
	}
	return events, nil
}

// decodeEventLine reads one log line as an event, false for a blank line or
// one that is not an event. A line over the scanner's ceiling is reported by
// the scanner, not skipped here.
func decodeEventLine(line string) (Event, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Event{}, false
	}
	var event Event
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return Event{}, false
	}
	if event.Type == "" {
		return Event{}, false
	}
	return event, true
}
