package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat"
)

// A seat kept because the jev_rules battery had not asked about its submission
// yet is offered to the end again on later passes (gt-381oj). The landing's
// own run of the end happens once, so the seat's landing facts — the bead, the
// branch, and the stamp the rules cursor is compared against — are recorded
// here, one file per seat under the town's runtime directory. The record is
// removed the moment an end decides anything else about the seat: removed,
// kept for a fact that stays true, or not a sandbox seat any more. A record an
// end cannot settle (a fact it could not measure) is retried on each heartbeat
// and goes once its one-hour window has closed, so no record outlives the seat
// it was waiting on.

// pendingSandboxSeatEnd is one recorded seat end. Rig and Name are the file it
// was read from; the file holds only the landing, since the path is the
// identity of the seat.
type pendingSandboxSeatEnd struct {
	Rig     string             `json:"-"`
	Name    string             `json:"-"`
	Landing sandboxSeatLanding `json:"landing"`
}

// pendingSandboxSeatEndsDir is <town>/.runtime/sandbox-seat-ends.
func pendingSandboxSeatEndsDir(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "sandbox-seat-ends")
}

// pendingSandboxSeatEndPath names the record of one seat, refusing a rig or
// polecat name a path cannot hold.
func pendingSandboxSeatEndPath(townRoot, rig, name string) (string, error) {
	if err := polecat.CheckSandboxSeatNames(rig, name); err != nil {
		return "", err
	}
	return filepath.Join(pendingSandboxSeatEndsDir(townRoot), rig, name+".json"), nil
}

// writePendingSandboxSeatEnd records landing for the pass that offers the seat
// again. The write is atomic, so a crash leaves the previous record or this
// one, never half of either.
func writePendingSandboxSeatEnd(townRoot, rig, name string, landing sandboxSeatLanding) error {
	path, err := pendingSandboxSeatEndPath(townRoot, rig, name)
	if err != nil {
		return err
	}
	return atomicfile.EnsureDirAndWriteJSON(path, pendingSandboxSeatEnd{Landing: landing})
}

// removePendingSandboxSeatEnd forgets the record of rig/name, if there is one.
func removePendingSandboxSeatEnd(townRoot, rig, name string) error {
	path, err := pendingSandboxSeatEndPath(townRoot, rig, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// readPendingSandboxSeatEnds returns every recorded seat end under town, and
// the paths it could not read: a record that cannot be read or parsed is
// reported, never silently skipped. A directory that cannot be listed is an
// error, so a pass decides nothing on a store it could not open.
func readPendingSandboxSeatEnds(townRoot string) (records []pendingSandboxSeatEnd, bad []string, err error) {
	dir := pendingSandboxSeatEndsDir(townRoot)
	rigs, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, rigEntry := range rigs {
		rigPath := filepath.Join(dir, rigEntry.Name())
		if !rigEntry.IsDir() {
			bad = append(bad, rigPath)
			continue
		}
		entries, readErr := os.ReadDir(rigPath)
		if readErr != nil {
			bad = append(bad, rigPath)
			continue
		}
		for _, entry := range entries {
			path := filepath.Join(rigPath, entry.Name())
			name, isJSON := strings.CutSuffix(entry.Name(), ".json")
			if entry.IsDir() || !isJSON {
				continue // an atomic write's temp file, or something that is not a record
			}
			if err := polecat.CheckSandboxSeatNames(rigEntry.Name(), name); err != nil {
				bad = append(bad, path)
				continue
			}
			rec, ok := readPendingSandboxSeatEnd(path)
			if !ok {
				bad = append(bad, path)
				continue
			}
			rec.Rig, rec.Name = rigEntry.Name(), name
			records = append(records, rec)
		}
	}
	sortPendingSandboxSeatEnds(records)
	return records, bad, nil
}

// readPendingSandboxSeatEnd reads one record; ok is false when the file cannot
// be read or decoded.
func readPendingSandboxSeatEnd(path string) (pendingSandboxSeatEnd, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the town's own runtime path
	if err != nil {
		return pendingSandboxSeatEnd{}, false
	}
	var rec pendingSandboxSeatEnd
	if err := json.Unmarshal(data, &rec); err != nil {
		return pendingSandboxSeatEnd{}, false
	}
	return rec, true
}

// sortPendingSandboxSeatEnds orders the records by rig then seat, so one pass
// offers them in the same order however the directory lists them.
func sortPendingSandboxSeatEnds(records []pendingSandboxSeatEnd) {
	less := func(i, j int) bool {
		if records[i].Rig != records[j].Rig {
			return records[i].Rig < records[j].Rig
		}
		return records[i].Name < records[j].Name
	}
	sort.Slice(records, less)
}
