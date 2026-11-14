package estlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Read returns the records of the estimate log of townRoot in the order they
// were appended, and the number of lines it skipped because they did not
// decode as a Record. A malformed line is skipped and counted, never an error,
// so one torn write cannot hide the records around it; a blank line is neither
// returned nor counted. A line over 1 MiB is a read failure rather than a
// malformed line: the read stops with an error and returns no records. A log
// file that does not exist yet reads as no records, and any other read that
// cannot complete is an error, never an empty log.
func Read(townRoot string) ([]Record, int, error) {
	path := Path(townRoot)
	if path == "" {
		return nil, 0, ErrNoTownRoot
	}
	f, err := os.Open(path) //nolint:gosec // G304: the path is under the town root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("storypoints/estlog: opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only: a close error cannot lose a record

	var records []Record
	malformed := 0
	sc := bufio.NewScanner(f)
	// One embedding line is a vector of some thousands of float32s, well under
	// this ceiling; a line past it is a corrupt file, not a record to skip.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// A JSON null decodes without error into a nil pointer, so it is
		// counted with the lines that do not decode rather than kept as a zero
		// Record.
		var r *Record
		if json.Unmarshal([]byte(line), &r) != nil || r == nil {
			malformed++
			continue
		}
		records = append(records, *r)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("storypoints/estlog: reading %s: %w", path, err)
	}
	return records, malformed, nil
}
