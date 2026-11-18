package update

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// ReadHistory returns the newest n finished runs in dir's history.jsonl, newest
// first. dir is the update state directory, as Dir returns it. A count of zero
// or less returns no runs. A missing file is an empty list. A line that does not
// parse, that carries another schema version, or that is a partial last line of
// an append still in progress is skipped. The read takes no lock and writes
// nothing.
func ReadHistory(dir string, n int) ([]HistoryLine, error) {
	path := filepath.Join(dir, historyFileName)
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []HistoryLine{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open update history %s: %w", path, err)
	}
	defer f.Close()

	// newest holds the last n valid lines in file order, oldest first. The file
	// grows by one line per run, so only n lines are kept in memory.
	var newest []HistoryLine
	reader := bufio.NewReader(f)
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("read update history %s: %w", path, readErr)
		}
		// AppendHistory ends every line with a newline, so a final line without
		// one is a write still in progress and is skipped like a corrupt line.
		if readErr == nil {
			if line, ok := parseHistoryLine(raw); ok {
				newest = append(newest, line)
				if len(newest) > n {
					newest = newest[1:]
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	slices.Reverse(newest)
	if newest == nil {
		return []HistoryLine{}, nil
	}
	return newest, nil
}

// parseHistoryLine decodes one history line. It reports false for a line that
// does not parse or whose version is not the one this build reads.
func parseHistoryLine(raw []byte) (HistoryLine, bool) {
	var line HistoryLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return HistoryLine{}, false
	}
	if line.V != stateVersion {
		return HistoryLine{}, false
	}
	return line, true
}
