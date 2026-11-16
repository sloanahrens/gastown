package reworkstats

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
)

// ReadLandings reads every rig's landing ledger in dir, with the count of lines
// that are not records. A directory that does not exist is a town that has not
// landed yet, and no records; a file that will not open or read is an error
// naming it, so a report never reads a failed read as an empty week. It is the
// one reader of the ledgers for the rework command and the daemon's numbers
// step, so the two cannot disagree about which files count.
func ReadLandings(dir string) ([]landings.Record, int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading the landings directory %s: %w", dir, err)
	}
	var recs []landings.Record
	skipped := 0
	for _, entry := range entries {
		name := entry.Name()
		// The review log is a .jsonl in the same directory and is not a ledger;
		// the verdict store and the backoff snapshots are directories and other
		// file kinds.
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") || name == land.ReviewsFileName {
			continue
		}
		got, bad, err := (&landings.Reader{Path: filepath.Join(dir, name)}).ReadNew()
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", name, err)
		}
		recs = append(recs, got...)
		skipped += len(bad)
	}
	return recs, skipped, nil
}

// ReadReviews reads the town's om verdicts through the review log's own line
// scanner, skipping the queue lines that are not verdicts and counting the lines
// it cannot use, because a lost verdict reads as a bead that needed fewer
// rounds. A log that does not exist is a town whose landings were never
// reviewed, and no records.
func ReadReviews(path string) ([]land.ReviewRecord, int, error) {
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("opening the review records %s: %w", path, err)
	}
	defer func() { _ = fh.Close() }()
	var recs []land.ReviewRecord
	unreadable := 0
	_, err = land.ScanReviewLines(fh, func(raw []byte) {
		// The scanner hands a blank final line to every reader of a file that
		// ends in a newline; a blank line is no record, not an unreadable one.
		if len(raw) == 0 || land.IsQueueLine(raw) {
			return
		}
		var rec land.ReviewRecord
		if json.Unmarshal(raw, &rec) != nil {
			unreadable++
			return
		}
		recs = append(recs, rec)
	})
	if err != nil {
		return nil, 0, fmt.Errorf("reading the review records %s: %w", path, err)
	}
	return recs, unreadable, nil
}
