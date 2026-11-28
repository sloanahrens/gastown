package gocacheprog

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// Disk is a Store over one directory: each body sits at <2 hex>/<output id>-d
// and the JSON index naming it at <2 hex>/<action id>-a. A put writes the body
// before the index, each atomically, so a reader never sees an index naming a
// body that is not there.
type Disk struct {
	dir string
	now func() time.Time
}

// diskIndex is the index file beside a body: its output id, size and put time.
type diskIndex struct {
	OutputID []byte
	Size     int64
	Time     time.Time
}

// NewDisk returns a Disk storing under dir and timestamping puts with now.
func NewDisk(dir string, now func() time.Time) (*Disk, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("gocacheprog: disk store: %w", err)
	}
	return &Disk{dir: dir, now: now}, nil
}

// Get returns actionID's entry, or ErrMiss when the index or body is absent,
// the index does not decode or the body is not the size the index names.
func (d *Disk) Get(actionID []byte) (Entry, error) {
	if !validID(actionID) {
		return Entry{}, fmt.Errorf("gocacheprog: disk store: id %q is not a non-empty hex string", actionID)
	}
	data, err := os.ReadFile(d.file(actionID, "-a"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Entry{}, fmt.Errorf("gocacheprog: disk store: read index: %w", err)
	}
	var ix diskIndex
	if err != nil || json.Unmarshal(data, &ix) != nil || !validID(ix.OutputID) {
		return Entry{}, ErrMiss
	}
	bodyPath := d.file(ix.OutputID, "-d")
	fi, err := os.Stat(bodyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Entry{}, fmt.Errorf("gocacheprog: disk store: stat body: %w", err)
	}
	if err != nil || fi.Size() != ix.Size {
		return Entry{}, ErrMiss
	}
	return Entry{OutputID: ix.OutputID, Size: ix.Size, Time: ix.Time, DiskPath: bodyPath}, nil
}

// Put stores body for actionID under outputID and returns the body's path. A
// body already present at that size is reused, not rewritten.
func (d *Disk) Put(actionID, outputID, body []byte) (string, error) {
	if !validID(actionID) || !validID(outputID) {
		return "", fmt.Errorf("gocacheprog: disk store: action %q or output %q id is not a non-empty hex string", actionID, outputID)
	}
	bodyPath := d.file(outputID, "-d")
	size := int64(len(body))
	switch fi, err := os.Stat(bodyPath); {
	case err == nil && fi.Size() == size:
	case err == nil || errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
			return "", fmt.Errorf("gocacheprog: disk store: create shard: %w", err)
		}
		if err := atomicfile.WriteFile(bodyPath, body, 0o644); err != nil {
			return "", fmt.Errorf("gocacheprog: disk store: write body: %w", err)
		}
	default:
		return "", fmt.Errorf("gocacheprog: disk store: stat body: %w", err)
	}
	ix := diskIndex{OutputID: outputID, Size: size, Time: d.now()}
	if err := atomicfile.EnsureDirAndWriteJSON(d.file(actionID, "-a"), ix); err != nil {
		return "", fmt.Errorf("gocacheprog: disk store: write index: %w", err)
	}
	return bodyPath, nil
}

// file returns the file for a valid id: its first two characters name the shard.
func (d *Disk) file(id []byte, suffix string) string {
	return filepath.Join(d.dir, string(id[:2]), string(id)+suffix)
}

// validID reports whether id is a non-empty hex string.
func validID(id []byte) bool {
	_, err := hex.DecodeString(string(id))
	return len(id) > 0 && err == nil
}
