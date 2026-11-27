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

// tierSweepMarkYoung reports whether a sweep mark is still inside its run
// budget: younger than tierSweepRunBudget. An older mark is an interrupted
// cycle too stale to resume (gt-rp65v0, gt-vbdeh9).
func tierSweepMarkYoung(mark tierSweepInFlightMark, now time.Time) bool {
	return now.Sub(mark.Started) < tierSweepRunBudget
}

// tierSweepResumeSHA names the commit a cycle should sweep (gt-vbdeh9). A mark
// younger than tierSweepRunBudget is a sweep the daemon was interrupted under,
// so its commit is resumed instead of the newer tip whose result the
// interrupted run would lose; one log line names both commits when they differ.
// No mark, or one at or past the budget, gives tip. An unreadable mark is
// logged and gives tip too (gt-ejsew3).
func (d *Daemon) tierSweepResumeSHA(rig, tip string, now time.Time) string {
	mark, err := readTierSweepInFlight(d.config.TownRoot, rig)
	if errors.Is(err, fs.ErrNotExist) {
		return tip
	}
	if err != nil {
		d.logger.Printf("tier_sweep: %s: reading the in-flight mark: %v; sweeping the tip %s", rig, err, shortSHA(tip))
		return tip
	}
	if !tierSweepMarkYoung(mark, now) {
		return tip
	}
	if mark.SHA != tip {
		d.logger.Printf("tier_sweep: %s: resuming the interrupted sweep of %s, not the tip %s", rig, shortSHA(mark.SHA), shortSHA(tip))
	}
	return mark.SHA
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
