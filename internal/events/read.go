package events

import (
	"bufio"
	"encoding/json"
	"errors"
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
	return ReadBounded(townRoot, 0)
}

// ErrEventsLogTooLarge is ReadBounded's refusal of a log past its bound.
var ErrEventsLogTooLarge = errors.New("the events log is larger than the read bound")

// ReadBounded returns the town's events log in append order, the way Read does,
// but refuses a log larger than maxBytes with ErrEventsLogTooLarge rather than
// read it. maxBytes at or below zero reads whatever is there.
//
// It is for a reader that needs the whole log and must not pay for an unbounded
// one — a landing that asks when a bead was dispatched, say. Such a reader
// cannot use a window of the log: one that starts inside a bead's run would
// date the bead from a repeat sling and answer short of the truth, so a log
// past the bound is an error and not a partial view. A missing log is no
// events and no error, as Read has it.
func ReadBounded(townRoot string, maxBytes int64) ([]Event, error) {
	if townRoot == "" {
		return nil, nil
	}

	f, err := os.Open(filepath.Join(townRoot, EventsFile)) //nolint:gosec // G304: path is under the town root
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening events file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	// One byte past the bound is what proves the log is larger, and the read
	// stops there rather than scanning a file of any size.
	var body io.Reader = f
	counted := &countingReader{r: f}
	if maxBytes > 0 {
		body = io.LimitReader(counted, maxBytes+1)
	} else {
		body = counted
	}
	events, err := readFrom(body)
	if err != nil {
		return events, err
	}
	if maxBytes > 0 && counted.n > maxBytes {
		return nil, fmt.Errorf("%w: over %d bytes", ErrEventsLogTooLarge, maxBytes)
	}
	return events, nil
}

// countingReader counts the bytes read through it, so a bounded read can tell a
// log that fit from one that ran past the bound.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readFrom decodes every event from r.
func readFrom(r io.Reader) ([]Event, error) {
	scanner := bufio.NewScanner(r)
	// Event lines carry message bodies, so the default 64KiB token is not
	// enough for a long nudge. 1MiB is the ceiling the daemon's prune shares.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
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
