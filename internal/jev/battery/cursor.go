package battery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Cursor is the last item a pass asked about: its creation time, and its ID
// to break a tie on that time.
type Cursor struct {
	Time time.Time `json:"time"`
	ID   string    `json:"id,omitempty"`
}

// ReadCursor returns the saved cursor, and false when there is none yet.
func ReadCursor(path string) (Cursor, bool, error) {
	var c Cursor
	ok, err := ReadState(path, &c)
	if err != nil {
		return Cursor{}, false, err
	}
	return c, ok, nil
}

// WriteCursor saves the cursor through a temporary file that is synced before
// the rename, so a crash or a power loss leaves either the old cursor or the
// new one, never a zero-length or truncated file that every later pass fails
// to parse.
func WriteCursor(path string, c Cursor) error {
	return WriteState(path, c)
}

// ReadState decodes the JSON state saved at path into into, and reports false
// when there is none yet. A pass whose cursor is more than a time and an ID
// keeps its own type and reads and writes it through these.
func ReadState(path string, into any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the state: %w", err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		return false, fmt.Errorf("parsing the state %s: %w", path, err)
	}
	return true, nil
}

// WriteState saves v as JSON at path through a temporary file that is synced
// before the rename. The file is never left truncated, see WriteCursor.
func WriteState(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding the state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing the state: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the state: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing the state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing the state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving the state: %w", err)
	}
	return nil
}
