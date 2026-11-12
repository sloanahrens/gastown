package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat"
)

// A sandbox seat the daemon keeps for any reason but the jev_rules ask leaves a
// durable record of the keep: the seat, its bead, the reason the end gave, and
// when that reason began (gt-464nw6). Until now the keep was a log line only, so
// nothing could age it or name it in an attention item. One file per seat under
// the town's runtime directory. The record is removed when the end decides the
// seat is no longer kept. The jev_rules keep has its own record
// (sandbox_seat_end_pending.go) and is not written here.

// KeptSandboxSeat is one seat the daemon keeps for a reason other than the
// jev_rules ask: its rig and polecat name, the bead it finished, the keep reason
// the end gave, and when that reason began.
type KeptSandboxSeat struct {
	Rig    string
	Name   string
	Bead   string
	Reason string
	Since  time.Time
}

// keptSandboxSeatRecord is what one file holds. The path is the seat's identity.
type keptSandboxSeatRecord struct {
	Bead   string    `json:"bead"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// keptSandboxSeatsDir is <town>/.runtime/sandbox-seat-kept.
func keptSandboxSeatsDir(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "sandbox-seat-kept")
}

// keptSandboxSeatPath names the record of one seat, refusing a rig or polecat
// name a path cannot hold.
func keptSandboxSeatPath(townRoot, rig, name string) (string, error) {
	if err := polecat.CheckSandboxSeatNames(rig, name); err != nil {
		return "", err
	}
	return filepath.Join(keptSandboxSeatsDir(townRoot), rig, name+".json"), nil
}

// writeKeptSandboxSeat records the keep of rig/name. The write is atomic, so a
// crash leaves the previous record or this one, never half of either.
func writeKeptSandboxSeat(townRoot, rig, name string, rec keptSandboxSeatRecord) error {
	path, err := keptSandboxSeatPath(townRoot, rig, name)
	if err != nil {
		return err
	}
	return atomicfile.EnsureDirAndWriteJSON(path, rec)
}

// removeKeptSandboxSeat forgets the record of rig/name, if there is one.
func removeKeptSandboxSeat(townRoot, rig, name string) error {
	path, err := keptSandboxSeatPath(townRoot, rig, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// readKeptSandboxSeatRecord reads one record. A file that is not there returns
// an error that matches os.ErrNotExist; any other error means the file exists
// and cannot be read or decoded.
func readKeptSandboxSeatRecord(path string) (keptSandboxSeatRecord, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the town's own runtime path
	if err != nil {
		return keptSandboxSeatRecord{}, err
	}
	var rec keptSandboxSeatRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return keptSandboxSeatRecord{}, fmt.Errorf("decoding %s: %w", path, err)
	}
	return rec, nil
}

// KeptSandboxSeats is the seats the daemon keeps for a reason other than the
// jev_rules ask under town, read from their records. A record that cannot be
// read or decoded is returned in bad, by path, never skipped, and so is a rig
// directory that cannot be listed. The store's own directory that cannot be
// listed is the error. A town with no records is an empty result, not an error.
func KeptSandboxSeats(townRoot string) (seats []KeptSandboxSeat, bad []string, err error) {
	dir := keptSandboxSeatsDir(townRoot)
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
			if !isJSON {
				continue // an atomic write's temp file
			}
			if entry.IsDir() || polecat.CheckSandboxSeatNames(rigEntry.Name(), name) != nil {
				bad = append(bad, path)
				continue
			}
			rec, recErr := readKeptSandboxSeatRecord(path)
			if recErr != nil {
				bad = append(bad, path)
				continue
			}
			seats = append(seats, KeptSandboxSeat{Rig: rigEntry.Name(), Name: name, Bead: rec.Bead, Reason: rec.Reason, Since: rec.Since})
		}
	}
	sort.Slice(seats, func(i, j int) bool {
		if seats[i].Rig != seats[j].Rig {
			return seats[i].Rig < seats[j].Rig
		}
		return seats[i].Name < seats[j].Name
	})
	return seats, bad, nil
}
