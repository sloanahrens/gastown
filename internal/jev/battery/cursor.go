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
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Cursor{}, false, nil
	}
	if err != nil {
		return Cursor{}, false, fmt.Errorf("reading the cursor: %w", err)
	}
	var c Cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return Cursor{}, false, fmt.Errorf("parsing the cursor %s: %w", path, err)
	}
	return c, true, nil
}

// WriteCursor saves the cursor through a temporary file, so a crash never
// leaves a half-written cursor that would make a pass ask twice.
func WriteCursor(path string, c Cursor) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding the cursor: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the cursor directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing the cursor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving the cursor: %w", err)
	}
	return nil
}
