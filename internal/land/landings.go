package land

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// LandingsFile is a rig's append-only landing log: one JSON LandingRecord per
// line, written by the landing worker after the target's tip is read back.
// It is the record nothing reaps (MR wisps were reaped within a day, so no
// landing survived before D2) and the list the red-main owner and the
// branch-protection audit read "who landed what" from.
type LandingsFile struct {
	Path string
}

// RigLandingsFile is <town>/.runtime/landings/<rig>.jsonl.
func RigLandingsFile(townRoot, rig string) (*LandingsFile, error) {
	if rig == "" || strings.ContainsAny(rig, `/\`) || rig == "." || rig == ".." {
		return nil, fmt.Errorf("invalid rig name %q for a landings file", rig)
	}
	return &LandingsFile{Path: filepath.Join(constants.TownRuntimePath(townRoot), "landings", rig+".jsonl")}, nil
}

// Append writes rec as one line. The directory must be 0700-safe (owned by
// this user, not group- or world-writable) and the file must not be a
// symlink, so no other account can redirect or forge the log.
func (f *LandingsFile) Append(rec LandingRecord) error {
	dir := filepath.Dir(f.Path)
	if err := ensureLandingsDir(dir); err != nil {
		return err
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding landing record: %w", err)
	}
	line = append(line, '\n')
	fh, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|oNoFollow, 0o600)
	if err != nil {
		return fmt.Errorf("opening landings file: %w", err)
	}
	if _, err := fh.Write(line); err != nil {
		_ = fh.Close()
		return fmt.Errorf("appending landing record: %w", err)
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		return fmt.Errorf("syncing landings file: %w", err)
	}
	return fh.Close()
}

// Find returns the latest record for beadID landing head, if the file has one.
// Land uses it to finish a landing whose bead record was left incomplete.
func (f *LandingsFile) Find(beadID, head string) (LandingRecord, bool, error) {
	return f.latest(func(rec LandingRecord) bool { return rec.BeadID == beadID && rec.Head == head })
}

// LatestForBead returns the latest record for beadID whatever head it
// landed. The landing worker reads it before a landing, so a bead whose
// record was left incomplete is repaired rather than landed twice even when
// its branch has moved since.
func (f *LandingsFile) LatestForBead(beadID string) (LandingRecord, bool, error) {
	return f.latest(func(rec LandingRecord) bool { return rec.BeadID == beadID })
}

// Recent returns up to n of the file's last records, oldest first.
func (f *LandingsFile) Recent(n int) ([]LandingRecord, error) {
	var recs []LandingRecord
	_, _, err := f.latest(func(rec LandingRecord) bool {
		recs = append(recs, rec)
		if len(recs) > n {
			recs = recs[1:]
		}
		return false
	})
	return recs, err
}

// Since returns the records landed at or after t, oldest first. The
// attention queue's risk-path collector reads a window this way (gt-vsct7.4).
func (f *LandingsFile) Since(t time.Time) ([]LandingRecord, error) {
	var recs []LandingRecord
	_, _, err := f.latest(func(rec LandingRecord) bool {
		if !rec.LandedAt.Before(t) {
			recs = append(recs, rec)
		}
		return false
	})
	return recs, err
}

func (f *LandingsFile) latest(match func(LandingRecord) bool) (LandingRecord, bool, error) {
	fh, err := os.Open(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return LandingRecord{}, false, nil
	}
	if err != nil {
		return LandingRecord{}, false, fmt.Errorf("opening landings file: %w", err)
	}
	defer func() { _ = fh.Close() }()
	var found LandingRecord
	ok := false
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec LandingRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return LandingRecord{}, false, fmt.Errorf("reading landings file %s: %w", f.Path, err)
		}
		if match(rec) {
			found, ok = rec, true
		}
	}
	if err := sc.Err(); err != nil {
		return LandingRecord{}, false, fmt.Errorf("reading landings file %s: %w", f.Path, err)
	}
	return found, ok, nil
}
