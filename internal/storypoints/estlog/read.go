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

// Read returns the records of the estimate log of townRoot in append order and
// the number of non-blank lines it skipped because they were not a record of a
// bead.
func Read(townRoot string) ([]Record, int, error) {
	path := Path(townRoot)
	if path == "" {
		return nil, 0, ErrNoTownRoot
	}
	// A log that does not exist yet reads as no records; any other failure to
	// open it is an error, never an empty log.
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
	// this ceiling; a line past it is a corrupt file, so it fails the read
	// rather than counting as malformed.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			// A blank line is no record, and it is not a malformed one either.
			continue
		}
		// A line that does not decode as a record is skipped and counted rather
		// than failing the read, so one torn write cannot hide the records
		// around it. A JSON null decodes without error into a nil pointer, and
		// an empty or foreign object into a zero Record that names no bead;
		// each is counted with the malformed lines.
		var r *Record
		if json.Unmarshal([]byte(line), &r) != nil || r == nil || r.BeadID == "" {
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
