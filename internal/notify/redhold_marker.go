package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/attention"
)

// redHoldMarkerFile is the red-hold marker under attention.Dir: what was already
// pushed for the hold that stands, so a daemon restart does not push it again.
const redHoldMarkerFile = "red_hold_push.json"

// redHoldMarkerPath is where the marker lives for the town at townRoot.
func redHoldMarkerPath(townRoot string) string {
	return filepath.Join(attention.Dir(townRoot), redHoldMarkerFile)
}

// ReadRedHoldMarker returns the marker for the red hold at townRoot; a missing
// file is the zero marker, nothing sent. Any other read or parse failure is an
// error, never the zero marker, which would push again. Its caller is
// PushRedHold, which sends the owed pushes.
func ReadRedHoldMarker(townRoot string) (RedHoldMarker, error) {
	var m RedHoldMarker
	data, err := os.ReadFile(redHoldMarkerPath(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse %s: %w", redHoldMarkerFile, err)
	}
	return m, nil
}

// WriteRedHoldMarker replaces the marker file at townRoot with m, atomically and
// readable only by its owner; a failed write is an error. Its caller is
// PushRedHold, which records a push only after it is made, so a write that fails
// leaves that push to be sent again rather than lost.
func WriteRedHoldMarker(townRoot string, m RedHoldMarker) error {
	path := redHoldMarkerPath(townRoot)
	if err := atomicfile.EnsureDirAndWriteJSONWithPerm(path, m, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", redHoldMarkerFile, err)
	}
	return nil
}
