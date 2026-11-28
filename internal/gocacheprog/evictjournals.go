package gocacheprog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// readJournals reads every .log journal in dir's journal directory, ownPath
// included. protectFrom is the earliest start among the live journals other
// than ownPath: an entry last used since then could be gained, so the eviction
// must protect it. notLive lists the journals no process holds, whose records
// are complete. A missing directory is empty, while a listing failure or a
// journal ReadJournal refuses is an error, since a partial read could
// unprotect an entry.
func readJournals(dir, ownPath string) (records []Record, protectFrom time.Time, notLive []string, err error) {
	journalDir := filepath.Join(dir, "journal")
	entries, err := os.ReadDir(journalDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, time.Time{}, nil, fmt.Errorf("gocacheprog: journals in %s: %w", journalDir, err)
	}
	own := filepath.Clean(ownPath)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		path := filepath.Join(journalDir, e.Name())
		c, err := ReadJournal(path)
		if err != nil {
			return nil, time.Time{}, nil, err
		}
		records = append(records, c.Records...)
		if path == own {
			continue
		}
		if !c.Live {
			notLive = append(notLive, path)
		} else if protectFrom.IsZero() || c.Start.Before(protectFrom) {
			protectFrom = c.Start
		}
	}
	return records, protectFrom, notLive, nil
}
