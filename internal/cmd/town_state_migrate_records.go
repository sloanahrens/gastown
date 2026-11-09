package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// seedRecordRel is the record a seed writes under the town's daemon directory
// before it copies: the volume it seeded and what the copy set held. The record
// is daemon-owned, so the copy carries it, and a later copy onto a populated
// volume is allowed only when this record names that volume.
const seedRecordRel = "daemon/town-state-seed.json"

// cutoverRecordRel is the cut-over time gt-yjzepk.7's landing check reads. It is
// written only after a copy, its bd read and its verify have all passed.
const cutoverRecordRel = "daemon/town-state-cutover.json"

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

// writeRecordFile writes v as JSON to rel under the town, 0600, by a temporary
// file in the same directory and a rename, so a reader never sees half a record.
func writeRecordFile(town, rel string, v any) error {
	p := filepath.Join(town, filepath.FromSlash(rel))
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", rel, err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("writing %s: %w", rel, err)
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
