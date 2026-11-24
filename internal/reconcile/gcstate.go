package reconcile

import (
	"encoding/json"
	"fmt"
	"time"
)

// ParseGCState returns each database's last gc time from the daemon's
// maintenance state file, reading only its last_gc object; a file without one
// yields an empty map, and an unparsable state or time is an error.
func ParseGCState(data []byte) (map[string]time.Time, error) {
	var state struct {
		// Raw per entry so an unparsable time can name the database it belongs
		// to, which decoding into time.Time would lose.
		LastGC map[string]json.RawMessage `json:"last_gc"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse the maintenance state: %w", err)
	}
	lastGC := make(map[string]time.Time, len(state.LastGC))
	for db, raw := range state.LastGC {
		var at time.Time
		if err := json.Unmarshal(raw, &at); err != nil {
			return nil, fmt.Errorf("parse the last gc of %s: %w", db, err)
		}
		// time.Time.UnmarshalJSON reads null as the zero time and no error:
		// that dates the gc at year 1 instead of reporting none.
		if at.IsZero() {
			return nil, fmt.Errorf("parse the last gc of %s: not a time", db)
		}
		lastGC[db] = at
	}
	return lastGC, nil
}
