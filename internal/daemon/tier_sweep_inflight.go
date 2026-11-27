package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// tierSweepInFlightMark names the commit a sweep cycle is working on and when it
// started, so a restarted daemon resumes that commit instead of a newer tip
// whose result the interrupted run would lose (gt-vbdeh9).
type tierSweepInFlightMark struct {
	SHA     string    `json:"sha"`
	Started time.Time `json:"started"`
}

// tierSweepInFlightPath is one rig's mark, a sibling of its record, so a
// cycle's whole state sits in one directory.
func tierSweepInFlightPath(townRoot, rig string) string {
	return filepath.Join(filepath.Dir(tierSweepStatePath(townRoot, rig)), rig+".inflight.json")
}

// readTierSweepInFlight reads one rig's mark. No mark is fs.ErrNotExist; a mark
// that does not parse, or names no commit or no start time, is an error.
func readTierSweepInFlight(townRoot, rig string) (tierSweepInFlightMark, error) {
	var mark tierSweepInFlightMark
	path := tierSweepInFlightPath(townRoot, rig)
	data, err := os.ReadFile(path)
	if err != nil {
		return tierSweepInFlightMark{}, err
	}
	if err := json.Unmarshal(data, &mark); err != nil {
		return tierSweepInFlightMark{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if mark.SHA == "" || mark.Started.IsZero() {
		return tierSweepInFlightMark{}, fmt.Errorf("mark %s names no commit or start time", path)
	}
	return mark, nil
}

// writeTierSweepInFlight writes one rig's mark whole, by rename, so a reader
// never sees half of it.
func writeTierSweepInFlight(townRoot, rig string, mark tierSweepInFlightMark) error {
	return atomicfile.EnsureDirAndWriteJSON(tierSweepInFlightPath(townRoot, rig), mark)
}

// clearTierSweepInFlight removes one rig's mark; one already gone is no error.
func clearTierSweepInFlight(townRoot, rig string) error {
	if err := os.Remove(tierSweepInFlightPath(townRoot, rig)); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
