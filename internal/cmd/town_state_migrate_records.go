package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/townstate"
)

// seedRecordRel is the record a seed writes under the town's daemon directory
// before it copies: the volume it seeded and what the copy set held. The record
// is daemon-owned, so the copy carries it, and a later copy onto a populated
// volume is allowed only when this record names that volume.
const seedRecordRel = "daemon/town-state-seed.json"

// cutoverRecordRel is the cut-over record. It is written only after a copy, its
// bd read and its verify have all passed; townstate owns the path and the
// day-one checks read it from there.
const cutoverRecordRel = townstate.CutoverRecordRel

// seedRecord is what the seed record holds. Files and Bytes are counted before
// the record itself, which the copy carries as one more file.
type seedRecord struct {
	SeededAt time.Time `json:"seeded_at"`
	Volume   string    `json:"volume"`
	Files    int       `json:"files"`
	Bytes    int64     `json:"bytes"`
}

// cutoverRecord is the cut-over time file: the RFC 3339 UTC instant of the
// cut-over, the volume, and the seconds of the window it was measured over,
// which runs from the copy's first read to the end of its verify, so the bd read
// and the verify are inside it.
type cutoverRecord struct {
	CutoverAt   string  `json:"cutover_at"`
	Volume      string  `json:"volume"`
	CopySeconds float64 `json:"copy_seconds"`
}

// readSeedRecord reads the town's seed record. An absent record is ok false, not
// an error; a record that does not parse is an error, never a missing seed.
func readSeedRecord(town string) (seedRecord, bool, error) {
	var rec seedRecord
	data, err := os.ReadFile(filepath.Join(town, filepath.FromSlash(seedRecordRel)))
	if errors.Is(err, fs.ErrNotExist) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, fmt.Errorf("reading the seed record: %w", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, false, fmt.Errorf("reading the seed record: %w", err)
	}
	return rec, true, nil
}

// encodeRecord is a record's bytes: indented JSON with a trailing newline. A copy
// and a seed write the same bytes for the same record.
func encodeRecord(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// writeRecordFile writes v as JSON to rel under the town, 0600, by a temporary
// sibling and a rename, so a reader never sees half a record. The sibling's name
// is fixed: a copy writes one record at a time, and the rename stays on one
// filesystem.
func writeRecordFile(town, rel string, v any) error {
	data, err := encodeRecord(v)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", rel, err)
	}
	tmpRel := rel + ".tmp"
	if err := os.WriteFile(filepath.Join(town, filepath.FromSlash(tmpRel)), data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	if err := os.Rename(filepath.Join(town, filepath.FromSlash(tmpRel)), filepath.Join(town, filepath.FromSlash(rel))); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", rel, err), removeRecordFile(town, tmpRel))
	}
	return nil
}

// removeSeedRecord removes the seed record. A record already gone is not an
// error: a failed copy must leave no record that names its partial volume.
func removeSeedRecord(town string) error {
	return removeRecordFile(town, seedRecordRel)
}

// removeRecordFile removes the record at rel under the town. A record already
// gone is not an error.
func removeRecordFile(town, rel string) error {
	err := os.Remove(filepath.Join(town, filepath.FromSlash(rel)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", rel, err)
	}
	return nil
}
