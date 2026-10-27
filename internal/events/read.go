package events

import (
	"bufio"
	"encoding/json"
	"fmt"
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

	var events []Event
	scanner := bufio.NewScanner(f)
	// Event lines carry message bodies, so the default 64KiB token is not
	// enough for a long nudge. 1MiB is the ceiling the daemon's prune shares.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
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
