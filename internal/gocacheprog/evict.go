package gocacheprog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Evict runs one eviction of the store in dir so its bodies total at most maxBytes. ownJournal is
// the caller's journal, already closed. A refused gate returns the zero summary and ran false. ran
// is true once the snapshot is written: an earlier failure keeps every journal and returns ran
// false; a failed journal removal after it returns the summary, ran true and the error.
func Evict(dir string, maxBytes int64, ownJournal string, now time.Time) (Summary, bool, error) {
	return evict(dir, maxBytes, ownJournal, now, nil)
}

// evict is Evict; beforeEvict, when set, runs before any chosen file moves, so a test can stand
// in for a process that writes during the eviction.
func evict(dir string, maxBytes int64, ownJournal string, now time.Time, beforeEvict func()) (Summary, bool, error) {
	release, run, err := evictGate(dir, now)
	if err != nil || !run {
		return Summary{}, false, err
	}
	defer release()

	indexPath := filepath.Join(dir, "index.jsonl")
	snapshot, err := ReadSnapshot(indexPath)
	if err != nil {
		return Summary{}, false, err
	}
	records, protectFrom, notLive, err := readJournals(dir, ownJournal)
	if err != nil {
		return Summary{}, false, err
	}
	first := make(map[Record]bool, len(records))
	for _, r := range records {
		first[utcRecord(r)] = true
	}
	idx := Merge(snapshot, records)
	actions, outputs, plan, err := Plan(idx, maxBytes, protectFrom, now)
	if err != nil {
		return Summary{}, false, err
	}

	// gained keeps the actions the reread names, so an entry journaled since the first read stays.
	var gained [][]byte
	reread := gainedSince(dir, ownJournal, first, idx)
	if beforeEvict != nil {
		beforeEvict()
	}
	deletedActions, deletedOutputs, err := applyEviction(dir, actions, outputs, func() (gainedActions, gainedOutputs [][]byte, err error) {
		gainedActions, gainedOutputs, err = reread()
		gained = gainedActions
		return gainedActions, gainedOutputs, err
	})
	if err != nil {
		return Summary{}, false, err
	}

	// The summary counts what applyEviction deleted, with each body's size from the first read.
	sizes := make(map[string]int64, len(idx))
	for _, e := range idx {
		sizes[e.OutputID] = e.Size
	}
	plan.EvictedEntries, plan.EvictedBytes = len(deletedActions), 0
	for _, o := range deletedOutputs {
		plan.EvictedBytes += sizes[string(o)]
	}
	for _, a := range deletedActions {
		delete(idx, string(a))
	}
	// A chosen entry whose files are absent, whether or not it was deleted here, leaves the snapshot.
	if err := dropMissing(dir, idx, actions, gained); err != nil {
		return Summary{}, false, err
	}
	if err := WriteSnapshot(indexPath, idx); err != nil {
		return Summary{}, false, err
	}

	var errs []error
	for _, path := range append([]string{ownJournal}, notLive...) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("gocacheprog: evict: remove journal %s: %w", path, err))
		}
	}
	return plan, true, errors.Join(errs...)
}
