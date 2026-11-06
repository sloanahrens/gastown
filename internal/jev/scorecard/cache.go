package scorecard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// ErrNoScorecard is what Read reports when the cache file is absent: no
// refresh has run yet.
var ErrNoScorecard = errors.New("no scorecard yet")

// ErrCorruptScorecard is what Read reports when the cache file holds something
// other than a scorecard document.
var ErrCorruptScorecard = errors.New("corrupt scorecard")

// CachePath returns the town's cached scorecard document,
// <town>/.runtime/jev/scorecard.json.
func CachePath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "scorecard.json")
}

// CacheDoc is the one document the cache holds: when the refresh ran, the
// window it scored, and one block per battery.
type CacheDoc struct {
	ScoredAt  time.Time      `json:"scored_at"`
	Since     string         `json:"since"`
	Batteries []BatteryBlock `json:"batteries"`
}

// BatteryBlock is one battery's part of a cached scorecard: the report the
// scorecard built, when it was scored, and whether the refresh failed. A
// failed block whose cache held a good report keeps that report with Kept set
// and KeptSince naming when it was scored; one that never scored carries the
// failure alone.
type BatteryBlock struct {
	Name     string    `json:"name"`
	Report   *Report   `json:"report,omitempty"`
	ScoredAt time.Time `json:"scored_at"`
	Failed   bool      `json:"failed,omitempty"`
	Error    string    `json:"error,omitempty"`
	Kept     bool      `json:"kept,omitempty"`
	// KeptSince is the instant the kept report was scored. It is set only
	// with Kept.
	KeptSince *time.Time `json:"kept_since,omitempty"`
}

// KeepFailed returns block with the previous document's block for the same
// battery substituted when this refresh's scoring failed and that block holds
// a report: the report and its scored_at are the last good ones, Kept is set,
// and KeptSince names when the report was scored. A failed block with no good
// report before it is returned as it is; a block that did not fail is too.
func KeepFailed(prev CacheDoc, block BatteryBlock) BatteryBlock {
	if !block.Failed || block.Kept {
		return block
	}
	for _, old := range prev.Batteries {
		if old.Name != block.Name || old.Report == nil {
			continue
		}
		keptSince := old.ScoredAt
		if old.Kept && old.KeptSince != nil {
			keptSince = *old.KeptSince
		}
		block.Report = old.Report
		block.ScoredAt = old.ScoredAt
		block.Kept = true
		block.KeptSince = &keptSince
		return block
	}
	return block
}

// Cache reads and writes the scorecard cache file. Its zero value uses the
// real filesystem; a test sets WriteFile or ReadFile to stand in for a write
// that fails or a file that cannot be read.
type Cache struct {
	// WriteFile publishes the document's bytes at path. Nil means
	// atomicfile.WriteFile, which writes a temp file beside path and renames
	// it over, so a failed write leaves the previous file untouched.
	WriteFile func(path string, data []byte, perm os.FileMode) error
	// ReadFile reads the document's bytes at path. Nil means os.ReadFile.
	ReadFile func(path string) ([]byte, error)
}

// Write stores doc at path, creating the parent directory if it is missing.
func (c Cache) Write(path string, doc CacheDoc) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the scorecard cache: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the scorecard cache directory for %s: %w", path, err)
	}
	write := c.WriteFile
	if write == nil {
		write = atomicfile.WriteFile
	}
	if err := write(path, data, 0o644); err != nil {
		return fmt.Errorf("writing the scorecard cache %s: %w", path, err)
	}
	return nil
}

// Read loads the document at path. A missing file is ErrNoScorecard and a file
// that does not parse as a scorecard is ErrCorruptScorecard; both errors name
// the path.
func (c Cache) Read(path string) (CacheDoc, error) {
	read := c.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return CacheDoc{}, fmt.Errorf("reading the scorecard cache %s: %w", path, ErrNoScorecard)
		}
		return CacheDoc{}, fmt.Errorf("reading the scorecard cache %s: %w", path, err)
	}
	var doc CacheDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return CacheDoc{}, fmt.Errorf("%w %s: %w", ErrCorruptScorecard, path, err)
	}
	if doc.ScoredAt.IsZero() {
		return CacheDoc{}, fmt.Errorf("%w %s: no scored_at", ErrCorruptScorecard, path)
	}
	for _, b := range doc.Batteries {
		if b.Name == "" {
			return CacheDoc{}, fmt.Errorf("%w %s: a battery block has no name", ErrCorruptScorecard, path)
		}
		if b.ScoredAt.IsZero() {
			return CacheDoc{}, fmt.Errorf("%w %s: battery %q has no scored_at", ErrCorruptScorecard, path, b.Name)
		}
	}
	return doc, nil
}
