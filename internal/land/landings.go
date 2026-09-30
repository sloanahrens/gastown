package land

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating landings dir: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("checking landings dir: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("refusing landings dir %s: mode %v is not a private directory", dir, info.Mode())
	}
	if err := checkOwner(dir, info); err != nil {
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
		if rec.BeadID == beadID && rec.Head == head {
			found, ok = rec, true
		}
	}
	if err := sc.Err(); err != nil {
		return LandingRecord{}, false, fmt.Errorf("reading landings file %s: %w", f.Path, err)
	}
	return found, ok, nil
}
